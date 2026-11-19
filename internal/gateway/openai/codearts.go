package openai

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"wb2api/internal/jsonx"
	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream/codearts"
)

type codeArtsBudget struct {
	attempt time.Duration
	total   time.Duration
}

func codeArtsWaitBudget(payload *jsonx.Obj) codeArtsBudget {
	// Incoming JSON bytes provide a ceiling independent of the client's model
	// tokenizer. Reserve extra time for large CJK, code and cached prompts.
	raw, _ := jsonx.Marshal(payload)
	size := len(raw)
	switch {
	case size >= 1<<20:
		return codeArtsBudget{5 * time.Minute, 6 * time.Minute}
	case size >= 256<<10:
		return codeArtsBudget{3 * time.Minute, 4 * time.Minute}
	default:
		return codeArtsBudget{90 * time.Second, 2 * time.Minute}
	}
}

func codeArtsSessionKey(r *http.Request, payload *jsonx.Obj) string {
	if id := strings.TrimSpace(r.Header.Get("X-Session-Id")); id != "" {
		return id
	}
	id, _ := payload.Get("prompt_cache_key").(string)
	return strings.TrimSpace(id)
}

func (h *Handler) scopeCodeArtsSession(q *request) {
	if !realm.IsCodeArts(q.realm) || q.unscopedSessionKey == "" {
		return
	}
	h.normaliseModelName(q)
	raw, _ := json.Marshal([4]string{q.keyID, q.realm, q.model, q.unscopedSessionKey})
	q.sessionKey = fmt.Sprintf("codearts:%x", sha256.Sum256(raw))
}

// Use a completed discovery result as a per-account entitlement catalogue.
// A partial or not-yet-discovered catalogue leaves eligibility unknown.
func codeArtsAllowsModel(rec *accounts.Record, model string) bool {
	extra := codearts.Extra(rec)
	if model == "" || extra["models_at"] == nil || extra["models_error"] != nil {
		return true
	}
	models := codearts.ModelsOf(rec)
	if len(models) == 0 {
		return true
	}
	wire := codearts.WireName(rec, model)
	for _, candidate := range models {
		if strings.EqualFold(candidate, wire) {
			return true
		}
	}
	return false
}

func publicRequestError(message string) []byte {
	raw, _ := json.Marshal(map[string]string{"error_msg": message})
	return raw
}

func (h *Handler) codeArtsTimeout(q *request) {
	h.emitWithLatency(q.r.Context(), UsageEvent{
		At: q.start, Model: q.model, Account: q.at.lastUID, KeyID: q.keyID,
		Stream: q.stream, Outcome: "failed", Status: http.StatusGatewayTimeout,
		Elapsed: time.Since(q.start),
	})
	q.httpErr(http.StatusGatewayTimeout, "CodeArts first output deadline exceeded", "server_error")
}

type observedRefresher interface {
	RefreshObserved(context.Context, *pool.Account, *accounts.Record) error
}

func (h *Handler) refreshCodeArtsAfterRejection(q *request, acc *pool.Account, observed *accounts.Record) bool {
	if h.Refresher == nil || q.refreshed[acc.UID] {
		return false
	}
	q.refreshed[acc.UID] = true
	timeout := 8 * time.Second
	if !q.codeArtsDeadline.IsZero() {
		timeout = minDur(timeout, time.Until(q.codeArtsDeadline))
	}
	ctx, cancel := context.WithTimeout(q.r.Context(), timeout)
	defer cancel()
	start := time.Now()
	defer func() { noteCodeArtsPhase(q.r.Context(), "refresh", time.Since(start)) }()
	if r, ok := h.Refresher.(observedRefresher); ok {
		return r.RefreshObserved(ctx, acc, observed) == nil
	}
	return h.Refresher.Refresh(ctx, acc) == nil
}
