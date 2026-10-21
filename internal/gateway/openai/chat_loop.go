package openai

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"wb2api/internal/gateway/ratelimit"
	"wb2api/internal/jsonx"
	"wb2api/internal/pool"
	"wb2api/internal/risk"
	"wb2api/internal/store/accounts"
	"wb2api/internal/store/settings"
	"wb2api/internal/upstream"
	"wb2api/internal/upstream/identity"
)

// kind of the most recent failure, which decides how the final error is told.
type failKind int

const (
	failNone failKind = iota
	failHTTP
	failTransport
	failLocalNet
	failTLS
)

// attemptState is the failure bookkeeping of one client request across its
// failover attempts.
type attemptState struct {
	tried     map[string]struct{}
	avoid     map[string]struct{}
	charged   map[string]bool // an account is charged at most once per request
	transient int
	netHits   int
	skipped   int

	kind       failKind
	lastStatus int
	lastBody   []byte
	lastErr    error
	lastUID    string

	last429       bool
	last429Detail string
	last403Detail string
}

// charge records a failure on an account at most once per request: a retry
// round re-picks the same account, and charging each time would push a briefly
// flaky account straight past the breaker.
func (q *request) charge(acc *pool.Account, msg string, unknown bool) {
	if q.at.charged[acc.UID] {
		return
	}
	q.at.charged[acc.UID] = true
	now := time.Now()
	if unknown {
		acc.NoteUnknownFailure(msg, now)
	} else {
		acc.NoteFailure(msg, now)
	}
}

func (q *request) demote(h *Handler, acc *pool.Account) {
	if q.sessionKey != "" {
		h.Pool.Affinity().Demote(q.sessionKey, acc.UID)
	}
}

// httpErr reports an error in the style of the endpoint being served.
func (q *request) httpErr(status int, msg, typ string) {
	if q.anth {
		writeAnthropicError(q.w, status, msg)
		return
	}
	writeError(q.w, status, msg, typ)
}

// rateErr answers 429 with Retry-After in the endpoint's style.
func (q *request) rateErr(wait int, message, detail string) {
	if q.anth {
		writeAnthropicRateLimited(q.w, wait, message, detail)
		return
	}
	writeRateLimited(q.w, wait, message, detail)
}

// run serves one parsed request: payload is a chat-completions body.
func (h *Handler) run(w http.ResponseWriter, r *http.Request, start time.Time, key *settings.APIKey, payload *jsonx.Obj, opts runOpts) {
	q := &request{r: r, w: w, start: start, payload: payload, refreshed: map[string]bool{}, anth: opts.anthropic}
	q.at.tried = map[string]struct{}{}
	q.at.charged = map[string]bool{}
	applyBackgroundAlias(payload, h.logger())
	q.model, _ = payload.Get("model").(string)
	// The local model deny-list: refuse before touching the upstream (legacy
	// is_model_banned), in whatever envelope the endpoint speaks.
	if h.Settings != nil && h.Settings.ModelBanned(q.model) {
		q.httpErr(http.StatusBadRequest,
			fmt.Sprintf("模型 %s 已被本机代理封锁（依 ALLOWED_MODELS / BANNED_SUBSTRING 设定）。", q.model),
			"invalid_request_error")
		return
	}
	q.stream = jsonx.Truthy(payload.Get("stream"))
	if opts.forceStream {
		q.stream = opts.stream
	}
	q.sink = opts.sink
	explicit := requestRealm(r, key)
	pinned := PinnedFrom(r.Context())
	switch {
	case explicit != "":
		q.realm = explicit
	case pinned != nil:
		q.realm = pinned.Realm
	default:
		q.realm = h.activeRealm()
	}
	if key != nil {
		q.keyID = key.ID
		if msg := crossRealmMessage(q.model, q.realm, key); msg != "" {
			q.httpErr(http.StatusBadRequest, msg, "invalid_request_error")
			return
		}
		if !key.AllowsModel(q.model) {
			q.httpErr(http.StatusBadRequest, keyModelMessage(key, q.model), "invalid_request_error")
			return
		}
	} else if msg := crossRealmMessage(q.model, q.realm, nil); msg != "" {
		q.httpErr(http.StatusBadRequest, msg, "invalid_request_error")
		return
	}
	if q.stream {
		q.maxTPS = h.streamTPS(key)
	}
	// No client-side concurrency cap: the gateway forwards every request it
	// receives and lets the upstream decide. Per-key concurrency_limit remains
	// in the stored settings for compatibility but is not enforced.
	q.sessionKey = extractSessionKey(r, payload)
	if opts.sessionKey != "" {
		q.sessionKey = opts.sessionKey
	}
	q.turnID = identity.NewTurnRequestID()
	q.vscodeConv = identity.NewMessageID()
	h.reshape(q)
	// No client session id: derive one from the conversation's stable head so
	// every turn lands on the account that already holds its prefix.
	if q.sessionKey == "" {
		if msgs, ok := q.shaped.Get("messages").([]any); ok {
			q.sessionKey, q.page = h.affinity().route(msgs)
		}
	}
	if h.Sched != nil && q.sessionKey != "" {
		for _, uid := range h.Pool.Affinity().Recent(q.sessionKey) {
			if h.Sched.ShouldEscape(uid) {
				if q.at.avoid == nil {
					q.at.avoid = map[string]struct{}{}
				}
				q.at.avoid[uid] = struct{}{}
			}
		}
	}
	if h.Guards != nil {
		h.Guards.Apply(false)
	}
	// Remaining-usage priority (opt-in): push the estimate's weights into the
	// pool before the first pick; when off, clear any weights left behind.
	if h.Limits != nil {
		if h.Settings.RemainingPriority() {
			h.Pool.ApplyRemainingWeights(h.Limits.ScheduleWeights())
		} else {
			h.Pool.ApplyRemainingWeights(nil)
		}
	}

	if rem := h.guard().HoldRemaining(q.realm); rem > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(rem.Seconds())+1))
		h.emit(UsageEvent{At: start, Model: q.model, KeyID: q.keyID, Stream: q.stream, Outcome: "failed", Status: http.StatusTooManyRequests, Elapsed: time.Since(start)})
		q.httpErr(http.StatusTooManyRequests, risk.HoldMessage(rem), "rate_limit_error")
		return
	}

	autoSwitch := h.Settings.AutoSwitchProduct() && pinned == nil
	max := h.maxAttempts()
	if autoSwitch {
		max += maxProductSwitches
	}
	if pinned != nil {
		// a pinned route is served by that one account alone, with no failover
		ref := pinnedRef(r)
		if pinned.Realm != q.realm {
			q.httpErr(http.StatusConflict, fmt.Sprintf("pinned account '%s' belongs to realm %s, request targets %s", ref, pinned.Realm, q.realm), "invalid_request_error")
			return
		}
		if !pinned.Ready(q.model, time.Now()) {
			q.httpErr(http.StatusServiceUnavailable, fmt.Sprintf("pinned account '%s' is not usable right now: %s", ref, pinned.UnavailableReason(q.model, time.Now())), "invalid_request_error")
			return
		}
		max = 1
	}
	var forced *pool.Account // retry the same account (after refresh / prompt degrade)
	fallbackTried := false
	for {
		aborted := false
		for attempt := 0; attempt < max+2; attempt++ {
			if r.Context().Err() != nil {
				return
			}
			acc := forced
			forced = nil
			if acc == nil {
				if len(q.at.tried) >= max {
					break
				}
				if pinned != nil {
					acc = pinned
				} else {
					exclude := q.at.tried
					if len(q.at.avoid) > 0 {
						exclude = unionExclude(q.at.tried, q.at.avoid)
					}
					acc = h.Pool.PickForSession(q.realm, q.sessionKey, exclude, q.model, q.page)
				}
				if acc == nil {
					// transient trouble: give the pool a moment and go round again
					if q.at.transient > 0 && attempt < max+1 {
						clear(q.at.tried)
						if !sleepCtx(r.Context(), minDur(time.Duration(1500*q.at.transient)*time.Millisecond, 3*time.Second)) {
							return
						}
						continue
					}
					break
				}
			}
			q.at.tried[acc.UID] = struct{}{}
			q.at.lastUID = acc.UID
			release := func() {}
			if h.Limiter != nil && h.AccountInflight > 0 {
				rel, ok, lerr := h.Limiter.Acquire(r.Context(), acc.UID, h.AccountInflight, q.turnID)
				if lerr != nil {
					h.logger().Warn("account limiter unavailable; falling back to local limits", "account", short(acc.UID), "err", lerr)
				} else if !ok {
					// The account is full in another process. Do not spend a
					// failover attempt on it; remember it for this request and
					// let the picker try another warm/cold account.
					delete(q.at.tried, acc.UID)
					if q.at.avoid == nil {
						q.at.avoid = map[string]struct{}{}
					}
					q.at.avoid[acc.UID] = struct{}{}
					q.at.skipped++
					if q.at.skipped > h.Pool.Len()+max {
						break
					}
					attempt--
					continue
				} else if rel != nil {
					release = rel
				}
			}
			res := h.try(q, acc)
			release()
			switch res.kind {
			case done:
				return
			case retrySame:
				delete(q.at.tried, acc.UID)
				forced = acc
				continue
			case stop:
				h.finishError(q, acc, res)
				return
			case abort:
				aborted = true
			}
			if aborted {
				break
			}
		}
		// The free model was used up on every account: move to its paid sibling
		// and run the whole selection again (legacy open_upstream recursion).
		if !fallbackTried && h.fallbackToPaidModel(q) {
			fallbackTried = true
			forced = nil
			continue
		}
		h.fail(q)
		return
	}
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func unionExclude(a, b map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		out[k] = struct{}{}
	}
	for k := range b {
		out[k] = struct{}{}
	}
	return out
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
	q.httpErr(res.status, fmt.Sprintf("upstream %d: %s", res.status, detail), typ)
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
	abort                       // give up and report from the recorded failure state
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

func egressKey(rawURL, proxy string) string {
	host := rawURL
	if u, err := url.Parse(rawURL); err == nil {
		host = u.Scheme + "://" + u.Host
	}
	return host + "|" + proxy
}

// try performs one upstream attempt with acc.
func (h *Handler) try(q *request, acc *pool.Account) result {
	acc.Acquire()
	defer acc.Release()

	rec := acc.Record()
	product := identity.NormalizeProduct(rec.Product)
	enc, gz := q.bodyFor(product)

	hdrs := h.chatHeaders(q, rec, product, gz)
	cfg := h.Settings.UpstreamConfig()

	ctx, cancel := context.WithCancel(q.r.Context())
	defer cancel()
	tDo := time.Now()
	target := h.chatURL(acc, product)
	resp, err := h.Up.Wire(acc.UID, acc.Proxy()).Do(ctx, upstream.Request{
		Method: http.MethodPost, URL: target, Headers: hdrs,
		Body: enc.wire(gz), HeaderTimeout: cfg.HeaderTimeout,
	})
	if traceTimings {
		h.logger().Info("trace", "pre_ms", tDo.Sub(q.start).Milliseconds(), "upstream_headers_ms", time.Since(tDo).Milliseconds())
	}
	if err != nil {
		if q.r.Context().Err() != nil {
			return result{kind: done}
		}
		return h.transportError(q, acc, err, egressKey(target, acc.Proxy()))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return h.classify(q, acc, resp.StatusCode, eb, egressKey(target, acc.Proxy()))
	}
	acc.NoteResult(true)
	if lifted := acc.NoteSuccess(q.model); lifted != nil {
		h.persistRecord(lifted)
	}
	if h.Settings.EgressBackoff() {
		h.egress.success(egressKey(target, acc.Proxy()))
	}
	h.resetSwitch(acc.UID, q.model)
	idle := cfg.IdleTimeout
	if q.sink != nil {
		q.sink(q, acc, cancel, idle, resp.Body)
	} else if q.stream {
		h.pipeStream(q.w, q.r, cancel, idle, resp.Body, acc, q.model, q.keyID, q.effort(h), q.start, q.maxTPS)
	} else {
		h.aggregate(q.w, q.r, cancel, idle, resp.Body, acc, q.model, q.keyID, q.effort(h), q.start)
	}
	return result{kind: done}
}

// persistRecord stores a changed account record (best effort: a failed write
// only costs the state across a restart).
func (h *Handler) persistRecord(rec *accounts.Record) {
	if h.Persist == nil || rec == nil {
		return
	}
	if err := h.Persist(rec); err != nil {
		h.logger().Warn("account record not persisted", "account", short(rec.UID), "err", err)
	}
}

// chatHeaders builds the outbound headers of one attempt for the account's
// identity: the conversation id is stable per (account, session key), the turn
// request id is shared by every retry of one user turn.
func (h *Handler) chatHeaders(q *request, rec *accounts.Record, product string, gz bool) identity.Headers {
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
	return hdrs
}

// transportError files a failure that produced no HTTP status.
func (h *Handler) transportError(q *request, acc *pool.Account, err error, egKey string) result {
	acc.NoteResult(false)
	q.at.lastErr, q.at.lastStatus, q.at.lastBody = err, 0, nil
	q.demote(h, acc)
	now := time.Now()
	switch {
	case isTLSVerify(err):
		// one attempt is enough: the chain is the same for every account
		q.at.kind = failTLS
		return result{kind: abort}
	case isLocalNetwork(err):
		// the machine has no network: no byte left it, so nobody is charged
		q.at.kind = failLocalNet
		q.at.netHits++
		if q.at.netHits == 1 {
			h.logger().Warn("local network looks down; not charging any account", "err", err)
		}
		if !sleepCtx(q.r.Context(), minDur(time.Duration(300*q.at.netHits)*time.Millisecond, 1500*time.Millisecond)) {
			return result{kind: done}
		}
		return result{kind: retry}
	case isTransient(err):
		q.at.kind = failTransport
		q.at.transient++
		if h.Settings.EgressBackoff() {
			// a transport blip is the path's problem, not the account's
			wait := h.egress.note(egKey, now)
			h.logger().Warn("upstream connection hiccup, backing off the egress", "account", short(acc.UID), "err", err, "backoff", wait.Round(time.Millisecond))
			if !sleepCtx(q.r.Context(), minDur(wait, egressSleepMax)) {
				return result{kind: done}
			}
			return result{kind: retry}
		}
		q.charge(acc, "connection: "+truncate(err.Error(), 120), true)
		_, _, fails, degrade := acc.Streaks()
		h.logger().Warn("upstream connection hiccup, retrying", "account", short(acc.UID), "err", err, "fails", fails, "degrade", degrade)
		if !sleepCtx(q.r.Context(), minDur(time.Duration(600*q.at.transient)*time.Millisecond, 2*time.Second)) {
			return result{kind: done}
		}
		return result{kind: retry}
	}
	q.at.kind = failTransport
	q.charge(acc, truncate(err.Error(), 120), true)
	cool := 60 * time.Second
	if h.Pool.RealmLen(q.realm) <= 1 {
		cool = 3 * time.Second
	}
	acc.CoolDown(now.Add(cool))
	h.logger().Warn("upstream request failed", "account", short(acc.UID), "err", err, "cooling", cool)
	return result{kind: retry}
}

func short(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// classify decides what an upstream HTTP error means for scheduling.
func (h *Handler) classify(q *request, acc *pool.Account, status int, body []byte, egKey string) result {
	acc.NoteResult(false)
	now := time.Now()
	q.at.kind, q.at.lastStatus, q.at.lastBody, q.at.lastErr = failHTTP, status, body, nil
	switch {
	case status == http.StatusTooManyRequests:
		detail := truncate(string(body), 600)
		reset, hasReset := ratelimit.ParseReset(detail)
		q.at.last429, q.at.last429Detail = true, detail
		q.demote(h, acc)
		if ratelimit.IsAccountLevel(detail, hasReset) {
			wait := acc.NoteSoftRate("HTTP 429 (account soft rate)", now)
			soft, _, _, _ := acc.Streaks()
			h.logger().Warn("account soft-rate limited", "account", short(acc.UID), "scope", ratelimit.ScopePhrase(detail),
				"cooling", wait, "streak", soft)
			return result{kind: retry, status: status, body: body}
		}
		// No reset clock means nothing says whether the limit is on the credential
		// or on one model: the window goes on this model and grows on the short
		// ladder when the body is a recognised soft-limit shape.
		wait := 60 * time.Second
		var until time.Time
		if hasReset {
			until = reset
			if wait = reset.Sub(now); wait < time.Second {
				wait = time.Second
			}
		}
		if !hasReset && ratelimit.IsSoftShape(detail) {
			wait = acc.NoteUnscopedRate(now)
			h.logger().Warn("unscoped 429, parking the model", "account", short(acc.UID), "model", q.model,
				"for", wait, "streak", acc.UnscopedStreak())
		}
		acc.NoteModelError(q.model, until, wait, "HTTP 429 (model throttled)", detail, now)
		// Any model-scoped 429 that names a reset instant becomes a window the
		// scheduler can probe at that instant (legacy wb_window_probe).
		if hasReset && h.Windows != nil {
			h.Windows.Schedule(acc.UID, q.model, reset)
		}
		// ...and a budget sample for the remaining-usage estimate.
		if hasReset && h.Limits != nil {
			h.Limits.NoteLimit(acc.UID, q.model, acc.Record().Realm, reset, now)
		}
		if hasReset && ratelimit.IsCap(detail) {
			// only the quota-cap answer (6004) is a quota verdict worth persisting
			if rec, isNew := acc.NoteModelCap(q.model, reset, now); isNew && rec != nil {
				h.persistRecord(rec)
			}
		}
		if h.Settings.AutoSwitchProduct() && h.trySwitchProduct(acc, q.model) {
			// a new identity is a different quota line: let it be tried again
			delete(q.at.tried, acc.UID)
			acc.ClearError(q.model, true, now)
			return result{kind: retry, status: status, body: body}
		}
		h.logger().Warn("account throttled on model (429)", "account", short(acc.UID), "model", q.model, "retry_in", wait.Round(time.Second))
		return result{kind: retry, status: status, body: body}

	case status == http.StatusUnauthorized:
		q.demote(h, acc)
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
		cool := 60 * time.Second
		if h.Pool.RealmLen(q.realm) <= 1 {
			cool = 3 * time.Second
		}
		acc.NoteError("HTTP 401", "")
		acc.CoolDown(now.Add(cool))
		acc.MarkTokenBad(true) // needs a refresh or a re-login before it serves again
		h.logger().Warn("account rejected (HTTP 401), rotating", "account", short(acc.UID))
		return result{kind: retry, status: status, body: body}

	case status == http.StatusPaymentRequired:
		q.demote(h, acc)
		msg := truncate(string(body), 200)
		if msg == "" {
			msg = "HTTP 402 (insufficient credits)"
		}
		acc.NoteBalanceCooled(msg, now)
		h.logger().Warn("account out of credits (402), parked until 04:00", "account", short(acc.UID))
		return result{kind: retry, status: status, body: body}

	case status == http.StatusForbidden:
		detail := truncate(string(body), 600)
		if ratelimit.IsRestriction(detail) {
			// Account-level restriction: park the account (persisted) and do NOT
			// replay the request on other accounts - they share the device.
			if rec := acc.NoteRestricted(now, risk.RestrictedAccountCooldown); rec != nil {
				h.persistRecord(rec)
			}
			h.logger().Warn("account restricted by upstream (403 11140)", "account", short(acc.UID))
			q.demote(h, acc)
			q.at.last403Detail = detail
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
		q.demote(h, acc)
		q.at.last403Detail = detail
		return result{kind: stop, status: status, body: body}

	case status >= 500:
		if code := modelScopedErrorCode(body); code != 0 && modelScopedErrorCodes[code] {
			// A dead/unsupported model must not be charged to the account: the
			// body says the model/service is unavailable, not the credential.
			// Stop rotating (another account would hit the same model error).
			h.logger().Warn("upstream model-scoped error; not charging the account",
				"account", short(acc.UID), "model", q.model, "code", code, "status", status)
			return result{kind: stop, status: status, body: body}
		}
		q.at.transient++
		q.demote(h, acc)
		if h.Settings.EgressBackoff() && (status == 502 || status == 503 || status == 504) {
			// gateway-side blips say the path is unhappy, not the credential
			wait := h.egress.note(egKey, now)
			h.logger().Warn("upstream gateway error, backing off the egress", "status", status, "model", q.model, "backoff", wait.Round(time.Millisecond))
			if !sleepCtx(q.r.Context(), minDur(wait, egressSleepMax)) {
				return result{kind: done}
			}
			return result{kind: retry, status: status, body: body}
		}
		q.charge(acc, fmt.Sprintf("HTTP %d", status), false)
		_, _, fails, _ := acc.Streaks()
		h.logger().Warn("upstream 5xx, retrying", "status", status, "model", q.model, "fails", fails)
		return result{kind: retry, status: status, body: body}
	}
	return result{kind: stop, status: status, body: body}
}

// -------------------------------------------------------- final error report

// retryAfter is the shortest wait until any account of the realm can serve model.
func (h *Handler) retryAfter(realmName, model string) int {
	now := time.Now()
	best := time.Duration(0)
	for _, a := range h.Pool.All() {
		if a.Realm != realmName || !a.Enabled() || a.Record().AccessToken == "" {
			continue
		}
		if w := a.ThrottleWait(model, now); w > 0 && (best == 0 || w < best) {
			best = w
		}
	}
	if best == 0 {
		return 60
	}
	return int(best.Seconds())
}

// realmModelThrottled: accounts exist and are healthy but all are cooling this
// model. Only a model cooldown counts: a plain account cooldown usually comes
// from a transient network error and must not read as "rate limited".
func (h *Handler) realmModelThrottled(realmName, model string) (bool, int) {
	now := time.Now()
	n := 0
	min := time.Duration(0)
	for _, a := range h.Pool.All() {
		if a.Realm != realmName || !a.Enabled() || a.Record().AccessToken == "" {
			continue
		}
		n++
		var w time.Duration
		for _, mc := range a.ModelCooldowns(now) {
			if mc.Model == model {
				w = mc.Until.Sub(now)
			}
		}
		if w <= 0 {
			return false, 0
		}
		if min == 0 || w < min {
			min = w
		}
	}
	if n == 0 {
		return false, 0
	}
	return true, int(min.Seconds())
}

func untilLocalMidnight() int {
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
	s := int(next.Sub(now).Seconds())
	if s < 60 {
		s = 60
	}
	return s
}

// unavailableDetail lists why each account of a realm cannot serve (capped, so
// a huge pool does not produce a huge error).
func (h *Handler) unavailableDetail(realmName, model string) string {
	var all []*pool.Account
	for _, a := range h.Pool.All() {
		if a.Realm == realmName {
			all = append(all, a)
		}
	}
	if len(all) == 0 {
		return "；该区域没有任何账号"
	}
	now := time.Now()
	var reasons []string
	for _, a := range all {
		if r := a.UnavailableReason(model, now); r != "" {
			reasons = append(reasons, short(a.UID)+" "+r)
		}
	}
	if len(reasons) == 0 {
		return ""
	}
	sort.Strings(reasons)
	extra := ""
	if len(reasons) > 12 {
		extra = fmt.Sprintf("，…共 %d 个", len(reasons))
		reasons = reasons[:12]
	}
	return "；各账号：" + strings.Join(reasons, "，") + extra
}

// fail tells the client why no attempt succeeded, in the legacy order of
// precedence: TLS, local network, rate limit, content rejection, the last
// upstream status, a transport failure, and finally the no-account cases.
func (h *Handler) fail(q *request) {
	a := &q.at
	emit := func(status int, acc string) {
		h.emit(UsageEvent{At: q.start, Model: q.model, Account: acc, KeyID: q.keyID, Stream: q.stream,
			Outcome: "failed", Status: status, Elapsed: time.Since(q.start)})
	}
	if a.lastUID != "" || a.kind != failNone {
		switch {
		case a.kind == failTLS:
			emit(http.StatusBadGateway, a.lastUID)
			q.httpErr(http.StatusBadGateway, "upstream TLS verification failed: "+a.lastErr.Error()+
				" (a TLS-inspecting proxy needs its CA in WB_CA_BUNDLE)", "invalid_request_error")
			return
		case a.kind == failLocalNet:
			emit(http.StatusServiceUnavailable, a.lastUID)
			q.httpErr(http.StatusServiceUnavailable, fmt.Sprintf("本机网络不可用：%s - 请检查本机网络/代理后重试（本次不计入账号熔断）",
				truncate(a.lastErr.Error(), 180)), "invalid_request_error")
			return
		case a.last429:
			emit(http.StatusTooManyRequests, a.lastUID)
			q.rateErr(h.retryAfter(q.realm, q.model), "", a.last429Detail)
			return
		case a.last403Detail != "":
			emit(http.StatusForbidden, a.lastUID)
			q.httpErr(http.StatusForbidden, "upstream 403: "+a.last403Detail, "invalid_request_error")
			return
		case a.kind == failHTTP:
			emit(a.lastStatus, a.lastUID)
			q.httpErr(a.lastStatus, fmt.Sprintf("upstream %d: %s", a.lastStatus, truncate(string(a.lastBody), 600)), "server_error")
			return
		case a.kind == failTransport:
			emit(http.StatusBadGateway, a.lastUID)
			q.httpErr(http.StatusBadGateway, fmt.Sprintf("upstream unreachable: %v", a.lastErr), "server_error")
			return
		}
	}
	if throttled, wait := h.realmModelThrottled(q.realm, q.model); throttled {
		emit(http.StatusTooManyRequests, "")
		q.rateErr(wait, "", "usage exceeds frequency limit")
		return
	}
	if h.Guards != nil {
		if b, ok := h.Guards.AllBlocked(q.realm, q.model); ok {
			var reason string
			switch b.Kind {
			case "token":
				reason = fmt.Sprintf("every usable account reached today's token limit (%d per account); the pool resumes after local midnight", b.Limit)
			case "credit":
				reason = fmt.Sprintf("every usable account reached today's credit limit (%d per account); paid models resume after local midnight, free models keep working", b.Limit)
			default:
				reason = fmt.Sprintf("every usable account reached today's token limit for %s (%d per account); the model resumes after local midnight", q.model, b.Limit)
			}
			emit(http.StatusTooManyRequests, "")
			q.rateErr(untilLocalMidnight(), reason, "")
			return
		}
	}
	emit(http.StatusServiceUnavailable, "")
	msg := fmt.Sprintf("no usable account for realm '%s': all are disabled, cooling down, expired or parked by the daily token limit%s - add or enable one at the dashboard (/)",
		q.realm, h.unavailableDetail(q.realm, q.model))
	q.httpErr(http.StatusServiceUnavailable, msg, "server_error")
}

// ---------------------------------------------------- outbound identity switch

const maxProductSwitches = 4

type switchEntry struct {
	count int
	last  time.Time
}

type switchLog struct {
	mu sync.Mutex
	m  map[string]switchEntry
}

func (s *switchLog) count(uid, model string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.m[uid+"\x00"+model]
	if time.Since(e.last) > time.Minute {
		return 0
	}
	return e.count
}

func (h *Handler) resetSwitch(uid, model string) {
	h.sw.mu.Lock()
	delete(h.sw.m, uid+"\x00"+model)
	h.sw.mu.Unlock()
}

// trySwitchProduct moves a rate-limited account to the next allowed outbound
// identity (workbuddy -> vscode -> cli): another identity is another quota
// line. At most maxProductSwitches per request window.
func (h *Handler) trySwitchProduct(acc *pool.Account, model string) bool {
	n := h.sw.count(acc.UID, model)
	if n >= maxProductSwitches {
		return false
	}
	current := identity.NormalizeProduct(acc.Record().Product)
	allowed := h.Settings.AutoSwitchProducts()
	seq := []string{identity.ProductDesktop, identity.ProductVSCode, identity.ProductCLI}
	start := -1
	for i, p := range seq {
		if p == current {
			start = i
		}
	}
	target := ""
	for off := 1; off <= len(seq); off++ {
		c := seq[(start+off+len(seq))%len(seq)]
		if c == current {
			continue
		}
		for _, a := range allowed {
			if a == c {
				target = c
			}
		}
		if target != "" {
			break
		}
	}
	if target == "" {
		return false
	}
	rec, ok := h.Pool.Update(acc.UID, func(r *accounts.Record) { r.Product = target })
	if !ok {
		return false
	}
	h.persistRecord(rec)
	h.sw.mu.Lock()
	if h.sw.m == nil || len(h.sw.m) > 500 {
		h.sw.m = map[string]switchEntry{}
	}
	h.sw.m[acc.UID+"\x00"+model] = switchEntry{count: n + 1, last: time.Now()}
	h.sw.mu.Unlock()
	h.logger().Warn("429: switching outbound identity", "account", short(acc.UID), "from", current, "to", target, "n", n+1)
	return true
}
