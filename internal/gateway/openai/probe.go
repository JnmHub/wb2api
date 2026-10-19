package openai

import (
	"context"
	"encoding/json"
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
	"wb2api/internal/risk"
	"wb2api/internal/upstream"
	"wb2api/internal/upstream/identity"
)

// TestModelDefault is the cheapest model every exit has.
const TestModelDefault = "deepseek-v4.1-flash"

// ProbeModel sends the quota-window probe: one minimal request through the
// account's own transport and identity. It reports whether the line serves
// again (ok) or when the upstream says it resets (zero = no clock given).
// It is intentionally state-light: a successful probe only clears that model's
// cooldown; a failing one is handled by the caller's retry ladder.
func (h *Handler) ProbeModel(ctx context.Context, acc *pool.Account, model string) (bool, time.Time, string) {
	rec := acc.Record()
	if exp := acc.ExpiresAt(); exp != 0 && time.Until(time.Unix(exp, 0)) < 120*time.Second && h.Refresher != nil {
		rctx, cancel := context.WithTimeout(ctx, 35*time.Second)
		_ = h.Refresher.Refresh(rctx, acc)
		cancel()
		rec = acc.Record()
	}

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
	product := identity.NormalizeProduct(rec.Product)
	enc, gz := q.bodyFor(product)
	hdrs := h.chatHeaders(q, rec, product, gz)

	cctx, cancel := context.WithTimeout(ctx, quota.ProbeTimeout)
	defer cancel()
	target := h.chatURL(acc, product)
	resp, err := h.Up.Wire(acc.UID, acc.Proxy()).Do(cctx, upstream.Request{
		Method: http.MethodPost, URL: target, Headers: hdrs, Body: enc.wire(gz)})
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
	product := identity.NormalizeProduct(rec.Product)
	enc, gz := q.bodyFor(product)
	hdrs := h.chatHeaders(q, rec, product, gz)

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	target := h.chatURL(acc, product)
	resp, err := h.Up.Wire(acc.UID, acc.Proxy()).Do(cctx, upstream.Request{
		Method: http.MethodPost, URL: target, Headers: hdrs, Body: enc.wire(gz)})
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
	reply := strings.TrimSpace(col.content)
	if reply == "" {
		reply = strings.TrimSpace(col.reasoning)
	}
	if reply == "" {
		reply = "OK"
	}
	if r := []rune(reply); len(r) > 80 {
		reply = string(r[:77]) + "..."
	}
	res.Reply = reply
	if lifted := acc.NoteSuccess(model); lifted != nil {
		h.persistRecord(lifted)
	}
	res.OK = true
	return res
}

func itoa(n int) string {
	return strings.TrimSpace(jsonx.PyStr(jsonNum(n)))
}

func jsonNum(n int) json.Number { return json.Number(strconv.Itoa(n)) }
