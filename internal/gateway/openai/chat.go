// Package openai implements the OpenAI-compatible gateway endpoints.
//
// STATUS: /v1/chat/completions is functionally complete up to the pieces that
// need the model catalogue and the long tail of the account failure state
// machine (soft-rate ladder, circuit breaker, daily limits, session affinity);
// see docs/PROGRESS.md for the exact list.
package openai

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"wb2api/internal/gateway/leak"
	"wb2api/internal/gateway/shape"
	"wb2api/internal/guards"
	"wb2api/internal/jsonx"
	"wb2api/internal/pool"
	"wb2api/internal/quota"
	"wb2api/internal/risk"
	"wb2api/internal/store/accounts"
	"wb2api/internal/store/settings"
	"wb2api/internal/upstream"
	"wb2api/internal/upstream/identity"
	"wb2api/internal/usageest"
)

// ChatPath is the upstream chat endpoint.
const ChatPath = "/v2/chat/completions"

// TokenRefresher renews one account's token (see package refresh).
type TokenRefresher interface {
	Refresh(ctx context.Context, a *pool.Account) error
}

// Handler serves /v1/chat/completions.
// InflightLimiter is an optional cross-process per-account concurrency gate.
// A nil limiter keeps the local-only behavior.
type InflightLimiter interface {
	Acquire(ctx context.Context, accountID string, max int, member string) (func(), bool, error)
}

// WaitingInflightLimiter is implemented by limiters that can queue briefly for
// a slot instead of failing over as soon as an account is full.
type WaitingInflightLimiter interface {
	AcquireWait(ctx context.Context, accountID string, max int, member string, maxWait time.Duration) (func(), bool, error)
}

// SchedulerFeedback reports whether a warm account should be escaped because
// its recent TTFT/error feedback is poor.
type SchedulerFeedback interface {
	ShouldEscape(accountID string) bool
	Score(accountID string) (ttftMs, errRate float64, ok bool)
}

type Handler struct {
	Pool     *pool.Pool
	Up       *upstream.Manager
	Settings *settings.Store
	Log      *slog.Logger

	// Refresher renews a token after a 401 (nil = no refresh, rotate instead).
	Refresher TokenRefresher
	// Risk guards against upstream risk-control sweeps (nil = a default guard).
	Risk *risk.Guard
	// Degrade is the content-review fallback window for prompt modes.
	Degrade *shape.DegradeGate
	// Catalog supplies per-model defaults (nil = none).
	Catalog shape.Catalog
	// Persist stores a changed account record (nil = memory only).
	Persist func(*accounts.Record) error

	// UpstreamOverride replaces the realm's chat upstream (tests, mock).
	UpstreamOverride string
	// MaxBody is the request body ceiling (0 = 16 MiB).
	MaxBody int64
	// MaxAttempts bounds account failover per request (0 = 5).
	MaxAttempts int
	// FilterMode selects the leak filter policy (zero value = Precise).
	FilterMode leak.Mode
	// MaxHoldTime bounds how long the leak filter may withhold text (0 = 20s).
	MaxHoldTime time.Duration
	// OnUsage receives one event per request; must not block.
	OnUsage func(UsageEvent)
	// Limiter optionally enforces account concurrency across processes.
	Limiter InflightLimiter
	// AccountInflight is the per-account limit used with Limiter (0 = off).
	AccountInflight int
	// AccountWait bounds how long a request queues on one full account after
	// the non-blocking probe found every ready account full (0 = fail over
	// immediately).
	AccountWait time.Duration
	// KeyLimiter optionally shares per-API-key concurrency across replicas.
	// Nil falls back to a local process gate.
	KeyLimiter KeyLimiter
	// FirstOutputTimeout bounds how long an upstream stream may stay silent
	// (or send only setup events) before the attempt is treated as failed and
	// another account is tried. Headers are held until semantic output exists.
	// Zero disables the guard.
	FirstOutputTimeout time.Duration
	// Sched optionally lets sticky routing escape a slow/error-prone account.
	Sched SchedulerFeedback
	// DefaultRealm is the fallback when CurrentRealm gives none (intl).
	DefaultRealm string
	// CurrentRealm returns the panel's active realm, used when neither the API
	// key, X-Realm nor ?realm= picks one (nil = DefaultRealm).
	CurrentRealm func() string
	// Guards refreshes the daily / reserve limits before each pick and explains
	// a fully parked pool (nil = no limits).
	Guards *guards.Scanner
	// Windows is the model-quota clock the 429 handler feeds and the scheduler
	// probes (nil = no window tracking).
	Windows *quota.Windows
	// Limits estimates the remaining 24h quota per (account, model); nil = off.
	Limits *usageest.Estimator

	keyCache   atomic.Pointer[keyCache]
	keyMu      sync.Mutex
	keyGates   map[string]*keyGate
	promptMu   sync.Mutex
	promptFile string
	promptMod  time.Time
	promptBody string
	riskOnce   sync.Once
	degOnce    sync.Once
	affOnce    sync.Once
	aff        affinityConfig
	egress     egressTracker
	sw         switchLog
}

func (h *Handler) affinity() affinityConfig {
	h.affOnce.Do(func() { h.aff = affinityFromEnv() })
	return h.aff
}

type keyCache struct {
	rev  uint64
	keys []settings.APIKey
}

// activeKeys returns the enabled API keys, re-parsed only when settings.json
// actually changed.
func (h *Handler) activeKeys() []settings.APIKey {
	rev := h.Settings.Rev()
	if c := h.keyCache.Load(); c != nil && c.rev == rev {
		return c.keys
	}
	keys := h.Settings.APIKeys(false)
	h.keyCache.Store(&keyCache{rev: rev, keys: keys})
	return keys
}

func (h *Handler) maxBody() int64 {
	if h.MaxBody > 0 {
		return h.MaxBody
	}
	return 16 << 20
}

func (h *Handler) maxAttempts() int {
	if h.MaxAttempts > 0 {
		return h.MaxAttempts
	}
	return 5
}

func (h *Handler) logger() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

func (h *Handler) guard() *risk.Guard {
	h.riskOnce.Do(func() {
		if h.Risk == nil {
			h.Risk = risk.NewGuard()
		}
	})
	return h.Risk
}

func (h *Handler) degrade() *shape.DegradeGate {
	h.degOnce.Do(func() {
		if h.Degrade == nil {
			h.Degrade = &shape.DegradeGate{}
		}
	})
	return h.Degrade
}

type apiError struct {
	Message     string `json:"message"`
	Type        string `json:"type"`
	Code        int    `json:"code,omitempty"`
	GatewayHint string `json:"gateway_hint,omitempty"`
}

func writeError(w http.ResponseWriter, status int, msg, typ string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := marshalNoEscape(map[string]apiError{"error": {Message: msg, Type: typ, Code: status, GatewayHint: gatewayHint(status, msg)}})
	_, _ = w.Write(append(b, '\n'))
}

// writeRateLimited answers 429 with Retry-After so clients back off.
func writeRateLimited(w http.ResponseWriter, wait int, message, detail string) {
	if wait < 1 {
		wait = 60
	}
	text := message
	if text == "" {
		text = fmt.Sprintf("upstream rate limit reached for this model; retry in %ds", wait)
		if detail != "" {
			text += " - " + truncate(detail, 200)
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Retry-After", strconv.Itoa(wait))
	w.WriteHeader(http.StatusTooManyRequests)
	b, _ := marshalNoEscape(map[string]any{"error": map[string]any{
		"message": text, "type": "rate_limit_error", "code": 429, "retry_after": wait}})
	_, _ = w.Write(append(b, '\n'))
}

// KeyRequired reports whether a key must be presented: any key is configured
// and the operator has not switched key checking off.
func (h *Handler) KeyRequired() bool {
	return h.Settings != nil && !h.Settings.AuthDisabled() && len(h.activeKeys()) > 0
}

// Authenticate returns the matched key (nil when auth is not required).
func (h *Handler) Authenticate(r *http.Request) (key *settings.APIKey, ok bool) {
	return h.authenticate(r)
}

// authenticate returns the matched key (nil when auth is not required).
func (h *Handler) authenticate(r *http.Request) (key *settings.APIKey, ok bool) {
	// The settings page's "关闭 API Key 校验" switch wins over any key: it is the
	// only way to run the gateway open on a trusted network.
	if h.Settings != nil && h.Settings.AuthDisabled() {
		return nil, true
	}
	keys := h.activeKeys()
	if len(keys) == 0 {
		return nil, true
	}
	tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if tok == "" {
		tok = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	for i := range keys {
		if keys[i].Enabled && keys[i].Key != "" && keys[i].Key == tok {
			return &keys[i], true
		}
	}
	return nil, false
}

// extractSessionKey mirrors legacy extract_session_key.
func extractSessionKey(r *http.Request, payload *jsonx.Obj) string {
	for _, name := range []string{"X-Conversation-Id", "Conversation-Id", "X-Session-Id", "Session-Id"} {
		if v := r.Header.Get(name); v != "" {
			return strings.TrimSpace(v)
		}
	}
	for _, k := range []string{"conversation_id", "session_id"} {
		if v := payload.Get(k); jsonx.Truthy(v) {
			return strings.TrimSpace(jsonx.PyStr(v))
		}
	}
	if md, ok := payload.Get("metadata").(*jsonx.Obj); ok {
		if v := md.Get("conversation_id"); jsonx.Truthy(v) {
			return strings.TrimSpace(jsonx.PyStr(v))
		}
	}
	return ""
}

// ---------------------------------------------------------------- per request

// request is the state of one client request across failover attempts.
type request struct {
	r          *http.Request
	w          http.ResponseWriter
	start      time.Time
	payload    *jsonx.Obj
	model      string
	stream     bool
	keyID      string
	realm      string
	sessionKey string
	turnID     string // X-Conversation-Request-ID, one per user turn (all retries)
	vscodeConv string
	shaped     *jsonx.Obj
	enc        map[string]*encoded
	sink       sinkFunc // renders the upstream stream (nil = chat completions)
	page       *pool.Page
	maxTPS     float64
	at         attemptState
	effortVal  string
	effortDone bool
	anth       bool

	refreshed       map[string]bool
	degradedRetried bool
	restrictedRot   int
}

type encoded struct {
	plain []byte
	gz    []byte
}

func (h *Handler) promptText(cfg settings.PromptConfig) string {
	if cfg.Mode != "custom" && cfg.Mode != "append" {
		return ""
	}
	if cfg.File == "" {
		return shape.DefaultGatewayPrompt
	}
	h.promptMu.Lock()
	defer h.promptMu.Unlock()
	st, err := os.Stat(cfg.File)
	if err != nil {
		return "" // unreadable prompt file fails open to passthrough
	}
	if h.promptFile == cfg.File && h.promptMod.Equal(st.ModTime()) {
		return h.promptBody
	}
	b, err := os.ReadFile(cfg.File)
	if err != nil {
		return ""
	}
	h.promptFile, h.promptMod, h.promptBody = cfg.File, st.ModTime(), string(b)
	return h.promptBody
}

func (h *Handler) reshape(q *request) {
	cfg := h.Settings.PromptConfig()
	q.shaped = shape.Build(q.payload, shape.Options{
		PromptMode: cfg.Mode, PromptText: h.promptText(cfg),
		Degraded:         h.degrade().Active(time.Now()),
		PreserveContract: preserveContract(),
		Catalog:          h.Catalog,
		Logf: func(f string, a ...any) {
			h.logger().Info(fmt.Sprintf(f, a...))
		},
	})
	q.enc = map[string]*encoded{}
}

func preserveContract() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WB_PRESERVE_REQUEST_CONTRACT"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.Bytes()
}

// bodyFor returns the encoded body for an identity: the official CLI drops a few
// fields and adds cache_retention, and the CLI and VSCode identities gzip. Each
// variant is encoded (and compressed) once per request, not once per attempt.
func (q *request) bodyFor(product string) (*encoded, bool) {
	variant := "plain"
	if product == identity.ProductCLI {
		variant = "cli"
	}
	if e, ok := q.enc[variant]; ok {
		return e, product != identity.ProductDesktop
	}
	obj := q.shaped
	if variant == "cli" {
		obj = q.shaped.Clone()
		for _, f := range identity.CLIBodyDropFields {
			obj.Delete(f)
		}
		obj.Set("cache_retention", identity.CLICacheRetention)
	}
	b, err := jsonx.Marshal(obj)
	if err != nil {
		b = []byte("{}")
	}
	e := &encoded{plain: b}
	q.enc[variant] = e
	return e, product != identity.ProductDesktop
}

func (e *encoded) wire(gz bool) []byte {
	if !gz {
		return e.plain
	}
	if e.gz == nil {
		e.gz = gzipBytes(e.plain)
	}
	return e.gz
}

// ServeHTTP handles POST /v1/chat/completions.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	start := time.Now()
	key, ok := h.authenticate(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing or invalid API key", "invalid_request_error")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBody())
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload too large", "invalid_request_error")
			return
		}
		writeError(w, http.StatusBadRequest, "cannot read request body", "invalid_request_error")
		return
	}
	payload, err := jsonx.UnmarshalObj(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "request body must be a JSON object", "invalid_request_error")
		return
	}
	h.run(w, r, start, key, payload, runOpts{})
}

// sinkFunc renders a successful upstream stream to the client.
type sinkFunc func(q *request, acc *pool.Account, cancel context.CancelFunc, idle time.Duration, body io.Reader)

// runOpts lets another endpoint reuse the chat pipeline (account selection,
// failover, identity, error handling) with its own request translation and
// response rendering.
type runOpts struct {
	forceStream bool
	stream      bool
	sessionKey  string
	sink        sinkFunc
	anthropic   bool // report errors in the Anthropic envelope
}

func (h *Handler) emit(ev UsageEvent) {
	if h.OnUsage != nil {
		h.OnUsage(ev)
	}
}

var traceTimings = os.Getenv("WB_TRACE") == "1"

// RiskHold is how long a realm is paused after an upstream risk-control wave.
func (h *Handler) RiskHold(realmName string) time.Duration { return h.guard().HoldRemaining(realmName) }
