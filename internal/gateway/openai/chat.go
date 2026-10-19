// Package openai implements the OpenAI-compatible gateway endpoints.
//
// STATUS: vertical slice. The hot path (auth, account selection, per-account
// upstream transport, SSE pipe with the tool-call leak filter, usage events)
// is real; protocol shaping done by legacy build_upstream_body (system prompt
// modes, reasoning-effort mapping, stop/max_tokens normalisation, CLI/VSCode
// identity headers, gzip bodies) is NOT ported yet. See docs/PROGRESS.md.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"wb2api/internal/gateway/leak"
	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/settings"
	"wb2api/internal/upstream"
)

// UsageEvent describes one finished request (success or failure).
type UsageEvent struct {
	At        time.Time
	Model     string
	Account   string
	KeyID     string
	Stream    bool
	Outcome   string // completed | client_aborted | upstream_aborted | failed
	Status    int
	Elapsed   time.Duration
	TTFT      time.Duration // request start -> first upstream chunk
	Gen       time.Duration // first -> last upstream chunk
	HeldFor   time.Duration // time text was withheld by the leak filter
	Usage     json.RawMessage
	Estimated bool // usage was estimated locally (not reported by upstream)
	Chunks    int
}

// Handler serves /v1/chat/completions.
type Handler struct {
	Pool     *pool.Pool
	Up       *upstream.Manager
	Settings *settings.Store
	Log      *slog.Logger

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

	keyCache atomic.Pointer[keyCache]
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

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

func writeError(w http.ResponseWriter, status int, msg, typ string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]apiError{"error": {Message: msg, Type: typ}})
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
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "request body must be a JSON object", "invalid_request_error")
		return
	}
	keyID := ""
	rl := h.DefaultRealm
	if rl == "" {
		rl = realm.Intl
	}
	rl = realm.DetectModelRealm(req.Model, rl)
	if key != nil {
		keyID = key.ID
		if key.Realm != "" {
			rl = key.Realm
		}
		if !key.AllowsModel(req.Model) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("model %q is not allowed for this API key", req.Model), "invalid_request_error")
			return
		}
	}

	tried := map[string]struct{}{}
	var lastStatus int
	var lastBody []byte
	for attempt := 0; attempt < h.maxAttempts(); attempt++ {
		acc := h.Pool.Pick(rl, req.Model, tried)
		if acc == nil {
			break
		}
		tried[acc.UID] = struct{}{}
		res := h.try(w, r, acc, body, req.Model, req.Stream, keyID, start)
		switch res.kind {
		case done:
			return
		case passthrough:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(res.status)
			_, _ = w.Write(res.body)
			h.emit(UsageEvent{At: start, Model: req.Model, Account: acc.UID, KeyID: keyID, Stream: req.Stream,
				Outcome: "failed", Status: res.status, Elapsed: time.Since(start)})
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
	} else if lastStatus != 0 {
		status, msg = http.StatusBadGateway, fmt.Sprintf("upstream error %d: %s", lastStatus, truncate(string(lastBody), 200))
	}
	h.emit(UsageEvent{At: start, Model: req.Model, KeyID: keyID, Stream: req.Stream, Outcome: "failed", Status: status, Elapsed: time.Since(start)})
	writeError(w, status, msg, "server_error")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

var traceTimings = os.Getenv("WB_TRACE") == "1"

type resultKind int

const (
	done        resultKind = iota // response fully handled
	retry                         // try another account
	passthrough                   // hand this upstream error to the client
)

type result struct {
	kind   resultKind
	status int
	body   []byte
}

func (h *Handler) chatURL(acc *pool.Account) string {
	if h.UpstreamOverride != "" {
		return strings.TrimRight(h.UpstreamOverride, "/") + "/v2/chat/completions"
	}
	return realm.Get(acc.Realm).ChatUpstream + "/v2/chat/completions"
}

// try performs one upstream attempt with acc.
func (h *Handler) try(w http.ResponseWriter, r *http.Request, acc *pool.Account, body []byte, model string, stream bool, keyID string, start time.Time) result {
	acc.Acquire()
	defer acc.Release()

	rec := acc.Record()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	up, err := http.NewRequestWithContext(ctx, http.MethodPost, h.chatURL(acc), bytes.NewReader(body))
	if err != nil {
		return result{kind: retry, status: 502}
	}
	up.Header.Set("Content-Type", "application/json")
	up.Header.Set("Accept", "text/event-stream")
	up.Header.Set("Authorization", "Bearer "+rec.AccessToken)
	up.Header.Set("X-User-Id", rec.UID)
	up.Header.Set("X-Domain", rec.Domain)

	proxy := rec.ProxyLegacy
	tDo := time.Now()
	resp, err := h.Up.Client(acc.UID, proxy).Do(up)
	if traceTimings {
		h.logger().Info("trace", "pre_ms", tDo.Sub(start).Milliseconds(), "upstream_headers_ms", time.Since(tDo).Milliseconds())
	}
	if err != nil {
		if r.Context().Err() != nil {
			return result{kind: done}
		}
		acc.NoteResult(false)
		acc.CoolDown(time.Now().Add(5 * time.Second))
		h.logger().Warn("upstream request failed", "account", short(acc.UID), "err", err)
		return result{kind: retry, status: 502, body: []byte(err.Error())}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return h.classify(acc, model, resp.StatusCode, eb)
	}
	acc.NoteResult(true)
	if stream {
		h.pipeStream(w, r, cancel, resp.Body, acc, model, keyID, start)
	} else {
		h.aggregate(w, r, cancel, resp.Body, acc, model, keyID, start)
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
func (h *Handler) classify(acc *pool.Account, model string, status int, body []byte) result {
	acc.NoteResult(false)
	now := time.Now()
	switch {
	case status == http.StatusTooManyRequests:
		acc.CoolDownModel(model, now.Add(60*time.Second))
		return result{kind: retry, status: status, body: body}
	case status == http.StatusUnauthorized:
		acc.CoolDown(now.Add(60 * time.Second))
		return result{kind: retry, status: status, body: body}
	case status == http.StatusPaymentRequired:
		acc.CoolDown(now.Add(time.Hour))
		return result{kind: retry, status: status, body: body}
	case status == http.StatusForbidden:
		// Content review / account restriction: do not spray the same request
		// over other accounts; give the client the upstream answer.
		return result{kind: passthrough, status: status, body: body}
	case status >= 500:
		acc.CoolDown(now.Add(5 * time.Second))
		return result{kind: retry, status: status, body: body}
	}
	return result{kind: passthrough, status: status, body: body}
}

func (h *Handler) emit(ev UsageEvent) {
	if h.OnUsage != nil {
		h.OnUsage(ev)
	}
}
