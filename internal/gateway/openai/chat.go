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
	"wb2api/internal/jsonx"
	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/risk"
	"wb2api/internal/store/accounts"
	"wb2api/internal/store/settings"
	"wb2api/internal/upstream"
	"wb2api/internal/upstream/identity"
)

// ChatPath is the upstream chat endpoint.
const ChatPath = "/v2/chat/completions"

// TokenRefresher renews one account's token (see package refresh).
type TokenRefresher interface {
	Refresh(ctx context.Context, a *pool.Account) error
}

// Handler serves /v1/chat/completions.
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
	// DefaultRealm is used when neither the key nor the model decides (intl).
	DefaultRealm string

	keyCache   atomic.Pointer[keyCache]
	promptMu   sync.Mutex
	promptFile string
	promptMod  time.Time
	promptBody string
	riskOnce   sync.Once
	degOnce    sync.Once
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
	Message string `json:"message"`
	Type    string `json:"type"`
}

func writeError(w http.ResponseWriter, status int, msg, typ string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := marshalNoEscape(map[string]apiError{"error": {Message: msg, Type: typ}})
	_, _ = w.Write(append(b, '\n'))
}

// KeyRequired reports whether any API key is configured (and so every
// request must carry one).
func (h *Handler) KeyRequired() bool { return len(h.activeKeys()) > 0 }

// Authenticate returns the matched key (nil when auth is not required).
func (h *Handler) Authenticate(r *http.Request) (key *settings.APIKey, ok bool) {
	return h.authenticate(r)
}

// authenticate returns the matched key (nil when auth is not required).
func (h *Handler) authenticate(r *http.Request) (key *settings.APIKey, ok bool) {
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
	q := &request{r: r, w: w, start: start, payload: payload, refreshed: map[string]bool{}}
	q.model, _ = payload.Get("model").(string)
	q.stream = jsonx.Truthy(payload.Get("stream"))
	rl := h.DefaultRealm
	if rl == "" {
		rl = realm.Intl
	}
	q.realm = realm.DetectModelRealm(q.model, rl)
	if key != nil {
		q.keyID = key.ID
		if key.Realm != "" {
			q.realm = key.Realm
		}
		if !key.AllowsModel(q.model) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("model %q is not allowed for this API key", q.model), "invalid_request_error")
			return
		}
	}
	q.sessionKey = extractSessionKey(r, payload)
	q.turnID = identity.NewTurnRequestID()
	q.vscodeConv = identity.NewMessageID()
	h.reshape(q)

	if rem := h.guard().HoldRemaining(q.realm); rem > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(rem.Seconds())+1))
		h.emit(UsageEvent{At: start, Model: q.model, KeyID: q.keyID, Stream: q.stream, Outcome: "failed", Status: http.StatusTooManyRequests, Elapsed: time.Since(start)})
		writeError(w, http.StatusTooManyRequests, risk.HoldMessage(rem), "rate_limit_error")
		return
	}

	tried := map[string]struct{}{}
	var forced *pool.Account // retry the same account (after refresh / prompt degrade)
	var lastStatus int
	var lastBody []byte
	for attempt := 0; attempt < h.maxAttempts()+2; attempt++ {
		acc := forced
		forced = nil
		if acc == nil {
			if len(tried) >= h.maxAttempts() {
				break
			}
			if acc = h.Pool.Pick(q.realm, q.model, tried); acc == nil {
				break
			}
		}
		tried[acc.UID] = struct{}{}
		res := h.try(q, acc)
		switch res.kind {
		case done:
			return
		case retrySame:
			delete(tried, acc.UID)
			forced = acc
			continue
		case stop:
			h.finishError(q, acc, res)
			return
		case retry:
			lastStatus, lastBody = res.status, res.body
		}
		if r.Context().Err() != nil {
			return
		}
	}
	status := http.StatusServiceUnavailable
	msg := "no usable account for this model; add or enable one in the dashboard"
	if lastStatus == http.StatusTooManyRequests {
		status, msg = lastStatus, "all accounts are rate limited for this model"
		w.Header().Set("Retry-After", "60")
	} else if lastStatus != 0 {
		status, msg = http.StatusBadGateway, fmt.Sprintf("upstream error %d: %s", lastStatus, truncate(string(lastBody), 200))
	}
	h.emit(UsageEvent{At: start, Model: q.model, KeyID: q.keyID, Stream: q.stream, Outcome: "failed", Status: status, Elapsed: time.Since(start)})
	writeError(w, status, msg, "server_error")
}

// finishError relays an upstream error that must not be retried elsewhere.
func (h *Handler) finishError(q *request, acc *pool.Account, res result) {
	detail := truncate(string(res.body), 600)
	typ := "server_error"
	if res.status == http.StatusForbidden {
		typ = "invalid_request_error"
		if strings.TrimSpace(detail) == "" {
			detail = "content rejected"
		}
	}
	h.emit(UsageEvent{At: q.start, Model: q.model, Account: acc.UID, KeyID: q.keyID, Stream: q.stream,
		Outcome: "failed", Status: res.status, Elapsed: time.Since(q.start)})
	writeError(q.w, res.status, fmt.Sprintf("upstream %d: %s", res.status, detail), typ)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

type resultKind int

const (
	done      resultKind = iota // response fully handled
	retry                       // try another account
	retrySame                   // try the same account again (token refreshed / prompt degraded)
	stop                        // hand this upstream error to the client, no more attempts
)

type result struct {
	kind   resultKind
	status int
	body   []byte
}

func (h *Handler) chatURL(acc *pool.Account, product string) string {
	if h.UpstreamOverride != "" {
		return strings.TrimRight(h.UpstreamOverride, "/") + ChatPath
	}
	base, _ := identity.Endpoint(acc.Realm, product)
	return base + ChatPath
}

// try performs one upstream attempt with acc.
func (h *Handler) try(q *request, acc *pool.Account) result {
	acc.Acquire()
	defer acc.Release()

	rec := acc.Record()
	product := identity.NormalizeProduct(rec.Product)
	enc, gz := q.bodyFor(product)

	conv := ""
	if product == identity.ProductVSCode {
		conv = q.vscodeConv
	} else if q.sessionKey != "" {
		conv = identity.ConversationIDFor(rec.UID, q.sessionKey)
	}
	hdrs := identity.ChatHeaders(identity.ChatParams{
		Params: identity.Params{
			Product: product, Realm: rec.Realm, UID: rec.UID, Token: rec.AccessToken,
			ConversationID: conv, EnterpriseID: rec.EnterpriseID, TenantID: rec.EnterpriseID,
			Model: q.model, SessionKey: q.sessionKey, TurnRequestID: q.turnID,
		},
		DeviceToken: h.Settings.ResolveDeviceToken(),
	})
	if gz {
		hdrs.Set("Content-Encoding", identity.RequestContentEnc)
	}
	cfg := h.Settings.UpstreamConfig()

	ctx, cancel := context.WithCancel(q.r.Context())
	defer cancel()
	tDo := time.Now()
	resp, err := h.Up.Wire(acc.UID, rec.ProxyLegacy).Do(ctx, upstream.Request{
		Method: http.MethodPost, URL: h.chatURL(acc, product), Headers: hdrs,
		Body: enc.wire(gz), HeaderTimeout: cfg.HeaderTimeout,
	})
	if traceTimings {
		h.logger().Info("trace", "pre_ms", tDo.Sub(q.start).Milliseconds(), "upstream_headers_ms", time.Since(tDo).Milliseconds())
	}
	if err != nil {
		if q.r.Context().Err() != nil {
			return result{kind: done}
		}
		acc.NoteResult(false)
		acc.CoolDown(time.Now().Add(5 * time.Second))
		h.logger().Warn("upstream request failed", "account", short(acc.UID), "err", err)
		return result{kind: retry, status: http.StatusBadGateway, body: []byte(err.Error())}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return h.classify(q, acc, resp.StatusCode, eb)
	}
	acc.NoteResult(true)
	idle := cfg.IdleTimeout
	if q.stream {
		h.pipeStream(q.w, q.r, cancel, idle, resp.Body, acc, q.model, q.keyID, q.start)
	} else {
		h.aggregate(q.w, q.r, cancel, idle, resp.Body, acc, q.model, q.keyID, q.start)
	}
	return result{kind: done}
}

func short(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// classify decides what an upstream error status means for scheduling.
func (h *Handler) classify(q *request, acc *pool.Account, status int, body []byte) result {
	acc.NoteResult(false)
	now := time.Now()
	switch {
	case status == http.StatusTooManyRequests:
		acc.CoolDownModel(q.model, now.Add(60*time.Second))
		return result{kind: retry, status: status, body: body}

	case status == http.StatusUnauthorized:
		// The token may simply have expired before its JWT clock did: refresh
		// once per account per request and retry that same account.
		if h.Refresher != nil && !q.refreshed[acc.UID] {
			q.refreshed[acc.UID] = true
			rctx, cancel := context.WithTimeout(q.r.Context(), 35*time.Second)
			err := h.Refresher.Refresh(rctx, acc)
			cancel()
			if err == nil {
				h.logger().Info("HTTP 401, token refreshed; retrying the same account", "account", short(acc.UID))
				return result{kind: retrySame, status: status, body: body}
			}
			h.logger().Warn("token refresh after 401 failed", "account", short(acc.UID), "err", err)
		}
		acc.CoolDown(now.Add(60 * time.Second))
		return result{kind: retry, status: status, body: body}

	case status == http.StatusPaymentRequired:
		acc.CoolDown(now.Add(time.Hour))
		return result{kind: retry, status: status, body: body}

	case status == http.StatusForbidden:
		detail := string(body)
		if risk.IsAccountRestriction(detail) {
			// Account-level restriction: park the account (persisted) and do NOT
			// replay the request on other accounts - they share the device.
			if rec := acc.NoteRestricted(now, risk.RestrictedAccountCooldown); rec != nil && h.Persist != nil {
				if err := h.Persist(rec); err != nil {
					h.logger().Warn("restriction not persisted", "account", short(acc.UID), "err", err)
				}
			}
			h.logger().Warn("account restricted by upstream (403 11140)", "account", short(acc.UID))
			h.guard().Note(q.realm, acc.UID)
			if q.restrictedRot >= h.guard().Cfg.RotateMax {
				return result{kind: stop, status: status, body: body}
			}
			q.restrictedRot++
			return result{kind: retry, status: status, body: body}
		}
		// Content review. In passthrough/append prompt modes a system-prompt
		// fingerprint is the usual culprit: switch to the neutral prompt until
		// 00:00 CST and retry this turn once.
		if m := h.Settings.PromptConfig().Mode; !q.degradedRetried && (m == "passthrough" || m == "append") {
			q.degradedRetried = true
			h.degrade().Trigger(now)
			h.reshape(q)
			h.logger().Info("upstream 403 (content review) -> degraded prompt retry")
			return result{kind: retrySame, status: status, body: body}
		}
		return result{kind: stop, status: status, body: body}

	case status >= 500:
		acc.CoolDown(now.Add(5 * time.Second))
		return result{kind: retry, status: status, body: body}
	}
	return result{kind: stop, status: status, body: body}
}

func (h *Handler) emit(ev UsageEvent) {
	if h.OnUsage != nil {
		h.OnUsage(ev)
	}
}

var traceTimings = os.Getenv("WB_TRACE") == "1"
