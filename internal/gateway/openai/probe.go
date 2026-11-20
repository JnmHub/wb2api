package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wb2api/internal/gateway/ratelimit"
	"wb2api/internal/gateway/shape"
	"wb2api/internal/jsonx"
	"wb2api/internal/pool"
	"wb2api/internal/quota"
	"wb2api/internal/realm"
	"wb2api/internal/risk"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream"
	"wb2api/internal/upstream/codearts"
	"wb2api/internal/upstream/identity"
	"wb2api/internal/upstream/raccoon"
)

// TestModelDefault is the cheapest model every exit has.
const TestModelDefault = "deepseek-v4.1-flash"

// probeModel picks the model a probe should send. A caller (or the default)
// may name a model another vendor serves; on raccoon that request would be
// refused pre-flight and the probe would report a body error that says nothing
// about the account. So an unusable name is replaced by the channel's own first
// catalogued model.
func probeModel(rec *accounts.Record, model string) string {
	if rec == nil || !realm.IsRaccoon(rec.Realm) {
		return model
	}
	if wire, ok := raccoon.ResolveChatModel(rec, model); ok {
		return wire
	}
	// Empty or unusable: the channel's own cheap default, when it serves it.
	if wire, ok := raccoon.ResolveChatModel(rec, raccoon.ProbeModel); ok {
		return wire
	}
	if list := raccoon.PublicModelsOfRecord(rec); len(list) > 0 {
		return list[0]
	}
	if list := raccoon.ModelsOfRecord(rec); len(list) > 0 {
		return list[0]
	}
	return raccoon.DefaultModel
}

// ProbeModel sends the quota-window probe: one minimal request through the
// account's own transport and identity. It reports whether the line serves
// again (ok) or when the upstream says it resets (zero = no clock given).
// It is intentionally state-light: a successful probe only clears that model's
// cooldown; a failing one is handled by the caller's retry ladder.
func (h *Handler) ProbeModel(ctx context.Context, acc *pool.Account, model string) (bool, time.Time, string) {
	rec := acc.Record()
	if exp := acc.ExpiresAt(); !realm.IsCodeArts(rec.Realm) && exp != 0 && time.Until(time.Unix(exp, 0)) < 120*time.Second && h.Refresher != nil {
		rctx, cancel := context.WithTimeout(ctx, 35*time.Second)
		_ = h.Refresher.Refresh(rctx, acc)
		cancel()
		rec = acc.Record()
	}
	model = probeModel(rec, model)

	payload := jsonx.NewObj()
	payload.Set("model", model)
	m := jsonx.NewObj()
	m.Set("role", "user")
	m.Set("content", quota.ProbePrompt)
	payload.Set("messages", []any{m})
	payload.Set("stream", true)
	payload.Set("max_tokens", quota.ProbeMaxTokens)
	payload.Set("reasoning_effort", "none")

	q := &request{r: (&http.Request{}).WithContext(ctx), payload: payload, model: model, realm: rec.Realm,
		refreshed: map[string]bool{}, turnID: identity.NewTurnRequestID(), vscodeConv: identity.NewMessageID()}
	q.shaped = shape.Build(payload, shape.Options{Catalog: h.Catalog})
	q.enc = map[string]*encoded{}
	cctx, cancel := context.WithTimeout(ctx, quota.ProbeTimeout)
	defer cancel()
	if realm.IsCodeArts(rec.Realm) {
		body, closeProbe, err := h.openCodeArtsProbe(cctx, q, acc)
		if err != nil {
			var fault *codearts.ChatError
			if errors.As(err, &fault) {
				detail := string(redactBody(fault.Body, acc.Record()))
				if reset, ok := ratelimit.ParseReset(detail); ok {
					return false, reset, detail
				}
			}
			return false, time.Time{}, truncate(err.Error(), 200)
		}
		defer closeProbe()
		_, _ = io.Copy(io.Discard, io.LimitReader(body, 1<<20))
		return true, time.Time{}, ""
	}
	product := identity.NormalizeProduct(rec.Product)
	out := h.buildOutbound(q, rec, product)
	if out.err != nil {
		// A body the channel refuses (e.g. a model it does not serve) is a
		// pre-flight fact, not an upstream failure: report it and leave the
		// account alone. Sending an empty body would earn "request body empty
		// or json invalid" from the upstream and cool a healthy account.
		return false, time.Time{}, truncate(out.err.Error(), 200)
	}
	hdrs := h.chatHeaders(q, rec, product, out, "")
	target := h.chatURL(acc, product)
	resp, err := h.Up.Wire(acc.UID, acc.Proxy()).Do(cctx, upstream.Request{
		Method: http.MethodPost, URL: target, Headers: hdrs, Body: out.body})
	if err != nil {
		return false, time.Time{}, truncate(err.Error(), 200)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return true, time.Time{}, ""
	}
	eb, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
	detail := string(eb)
	if resp.StatusCode == http.StatusTooManyRequests {
		if reset, ok := ratelimit.ParseReset(detail); ok {
			return false, reset, detail
		}
	}
	return false, time.Time{}, "HTTP " + itoa(resp.StatusCode) + ": " + truncate(detail, 150)
}

// ProbeResult is one account test, shaped for the panel.
type ProbeResult struct {
	UID        string `json:"uid"`
	Nickname   string `json:"nickname"`
	Realm      string `json:"realm"`
	Model      string `json:"model"`
	OK         bool   `json:"ok"`
	ElapsedMs  int64  `json:"elapsed_ms"`
	Status     int    `json:"status,omitempty"`
	Reply      string `json:"reply,omitempty"`
	Error      string `json:"error,omitempty"`
	Network    bool   `json:"network,omitempty"`
	Restricted bool   `json:"restricted,omitempty"`
	Fails      int    `json:"fails,omitempty"`
}

// Probe sends one minimal request through the account and reports the result.
// It takes the same semantic paths as live traffic: a served request clears the
// account's local penalties (the next 429 no longer resumes an old ladder), a
// risk-control 403 is recorded as a restriction (never lifted by a green test),
// and a dead local network charges nobody.
func (h *Handler) Probe(ctx context.Context, acc *pool.Account, model string) ProbeResult {
	if model == "" {
		model = TestModelDefault
	}
	rec := acc.Record()
	model = probeModel(rec, model)
	nick := rec.Nickname
	if nick == "" {
		nick = short(rec.UID)
	}
	res := ProbeResult{UID: rec.UID, Nickname: nick, Realm: rec.Realm, Model: model}
	start := time.Now()
	payload := jsonx.NewObj()
	payload.Set("model", model)
	m := jsonx.NewObj()
	m.Set("role", "user")
	m.Set("content", "hi")
	payload.Set("messages", []any{m})
	payload.Set("stream", false)

	q := &request{r: (&http.Request{}).WithContext(ctx), start: start, payload: payload, model: model, realm: rec.Realm,
		refreshed: map[string]bool{}, turnID: identity.NewTurnRequestID(), vscodeConv: identity.NewMessageID()}
	q.shaped = shape.Build(payload, shape.Options{Catalog: h.Catalog})
	q.enc = map[string]*encoded{}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if realm.IsCodeArts(rec.Realm) {
		body, closeProbe, err := h.openCodeArtsProbe(cctx, q, acc)
		if err == nil {
			defer closeProbe()
			col := h.collect(q.r, cancel, 30*time.Second, body, model, false)
			err = col.err
			res.Reply = truncateProbeReply(col.content, col.reasoning)
		}
		res.ElapsedMs = time.Since(start).Milliseconds()
		if err != nil {
			res.Error = truncate(err.Error(), 200)
			var fault *codearts.ChatError
			if errors.As(err, &fault) {
				res.Status = fault.Status
			}
			return res
		}
		if lifted := acc.NoteSuccess(model); lifted != nil {
			h.persistRecord(lifted)
		}
		res.OK, res.Status = true, http.StatusOK
		return res
	}
	product := identity.NormalizeProduct(rec.Product)
	out := h.buildOutbound(q, rec, product)
	if out.err != nil {
		res.Error = truncate(out.err.Error(), 200)
		return res
	}
	hdrs := h.chatHeaders(q, rec, product, out, "")
	target := h.chatURL(acc, product)
	resp, err := h.Up.Wire(acc.UID, acc.Proxy()).Do(cctx, upstream.Request{
		Method: http.MethodPost, URL: target, Headers: hdrs, Body: out.body})
	res.ElapsedMs = time.Since(start).Milliseconds()
	if err != nil {
		switch {
		case isTLSVerify(err):
			res.Error = "TLS verification failed: " + err.Error()
		case isLocalNetwork(err):
			// no byte left this machine: report it, charge nobody
			res.Network = true
			res.Error = "本机网络不可用：" + truncate(err.Error(), 160)
			acc.NoteError(res.Error, "")
		default:
			res.Error = truncate(err.Error(), 200)
			// a diagnostic must show the failure but must not switch the account off
			acc.NoteError(truncate(err.Error(), 80), "")
			_, _, res.Fails, _ = acc.Streaks()
		}
		return res
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		detail := string(eb)
		res.Status = resp.StatusCode
		res.Error = "HTTP " + itoa(resp.StatusCode) + ": " + truncate(detail, 150)
		if resp.StatusCode == http.StatusForbidden && ratelimit.IsRestriction(detail) {
			if r := acc.NoteRestricted(time.Now(), risk.RestrictedAccountCooldown); r != nil {
				h.persistRecord(r)
			}
			h.guard().Note(rec.Realm, rec.UID)
			res.Restricted = true
			_, _, res.Fails, _ = acc.Streaks()
			return res
		}
		acc.CoolDown(time.Now().Add(60 * time.Second))
		acc.NoteError("HTTP "+itoa(resp.StatusCode)+": "+truncate(detail, 80), detail)
		_, _, res.Fails, _ = acc.Streaks()
		return res
	}
	col := h.collect(&http.Request{}, func() {}, 30*time.Second, resp.Body, model, true)
	res.ElapsedMs = time.Since(start).Milliseconds()
	if col.err != nil {
		res.Error = "stream error: " + truncate(col.err.Error(), 150)
		return res
	}
	res.Reply = truncateProbeReply(col.content, col.reasoning)
	if lifted := acc.NoteSuccess(model); lifted != nil {
		h.persistRecord(lifted)
	}
	res.OK = true
	return res
}

func truncateProbeReply(content, reasoning string) string {
	reply := strings.TrimSpace(content)
	if reply == "" {
		reply = strings.TrimSpace(reasoning)
	}
	if reply == "" {
		reply = "OK"
	}
	if r := []rune(reply); len(r) > 80 {
		reply = string(r[:77]) + "..."
	}
	return reply
}

func (h *Handler) openCodeArtsProbe(ctx context.Context, q *request, acc *pool.Account) (io.Reader, func(), error) {
	rec := acc.Record()
	if rec.CodeArtsNeedsLogin() {
		return nil, nil, errors.New("CodeArts 账号需重新登录")
	}
	if !rec.CodeArtsAccessUsable(time.Now()) && h.Refresher != nil {
		if err := h.Refresher.Refresh(ctx, acc); err != nil {
			return nil, nil, err
		}
		rec = acc.Record()
	}
	if !rec.CodeArtsAccessUsable(time.Now()) {
		return nil, nil, errors.New("CodeArts 签名凭据不可用")
	}
	product := identity.NormalizeProduct(rec.Product)
	out := h.buildOutbound(q, rec, product)
	if out.err != nil {
		return nil, nil, out.err
	}
	sessionID := ""
	closeLease := func() {}
	if h.Sessions != nil {
		lease, err := h.Sessions.Begin(ctx, acc)
		if err != nil {
			var fault *codearts.HeartbeatError
			if errors.As(err, &fault) {
				if fault.Status == http.StatusUnauthorized || codearts.IsExpiredAccessToken(fault.Body) {
					h.refreshCodeArtsAfterRejection(q, acc, rec)
				}
			}
			return nil, nil, err
		}
		sessionID, closeLease = lease.ID(), lease.Close
	}
	probeCtx, cancel := context.WithCancel(ctx)
	resp, err := h.Up.Wire(acc.UID, acc.Proxy()).Do(probeCtx, upstream.Request{
		Method: http.MethodPost, URL: h.chatURL(acc, product),
		Headers: h.chatHeaders(q, rec, product, out, sessionID), Body: out.body,
	})
	if err != nil {
		cancel()
		closeLease()
		return nil, nil, err
	}
	closeProbe := func() {
		cancel()
		_ = resp.Body.Close()
		closeLease()
	}
	q.toolNames = out.names
	reader := codeArtsReplyBody(resp.Body, resp.Header.Get("Content-Encoding"))
	if resp.StatusCode != http.StatusOK {
		body := readUpstreamErrorBody(reader, cancel, 2*time.Second)
		err = &codearts.ChatError{Status: resp.StatusCode, Body: body, Code: upstreamCodeOf(body)}
	} else {
		reader, err = h.stageCodeArtsOutput(q, reader)
	}
	if err != nil {
		closeProbe()
		var fault *codearts.ChatError
		if errors.As(err, &fault) && (fault.Status == http.StatusUnauthorized || codearts.IsExpiredAccessToken(fault.Body)) {
			h.refreshCodeArtsAfterRejection(q, acc, rec)
		}
		return nil, nil, err
	}
	return reader, closeProbe, nil
}

func itoa(n int) string {
	return strings.TrimSpace(jsonx.PyStr(jsonNum(n)))
}

func jsonNum(n int) json.Number { return json.Number(strconv.Itoa(n)) }
