package openai

import (
	"os"
	"strings"
	"time"

	"wb2api/internal/realm"
)

// freeModelCreditFallbacks maps a free model whose 24h window is used up to a
// paid sibling that draws on credits instead. Port of the legacy table: only
// the INTL flash model has one.
var freeModelCreditFallbacks = map[string]string{
	realm.Intl + "/deepseek-v4.1-flash": "deepseek-v4.1-flash-sg",
}

// freeFallbackEnabled: the settings switch, or the legacy env override.
func (h *Handler) freeFallbackEnabled() bool {
	if h.Settings != nil && h.Settings.FreeModelCreditFallback() {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WB_ENABLE_FREE_MODEL_CREDIT_FALLBACK"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// allCapped reports whether every eligible account of the realm has a persisted
// upstream 6004 window for model that has not expired yet.
func (h *Handler) allCapped(realmName, model string, now time.Time) bool {
	if h.Pool == nil {
		return false
	}
	eligible := 0
	for _, a := range h.Pool.All() {
		rec := a.Record()
		if rec.Realm != realmName || !rec.IsEnabled() || rec.AccessToken == "" {
			continue
		}
		eligible++
		if !a.CapUntil(model, now).After(now) {
			return false
		}
	}
	return eligible > 0
}

// fallbackToPaidModel rewrites the request onto the paid sibling when a free
// model's window has been used up on every account that could take it. It is
// called after the free model's attempts ran out; true means restart the
// attempt loop with the new model.
func (h *Handler) fallbackToPaidModel(q *request) bool {
	paid := freeModelCreditFallbacks[q.realm+"/"+q.model]
	if paid == "" || !h.freeFallbackEnabled() {
		return false
	}
	now := time.Now()
	if !h.allCapped(q.realm, q.model, now) {
		return false
	}
	h.logger().Warn("free model exhausted on every account, retrying the paid sibling",
		"realm", q.realm, "model", q.model, "paid_model", paid)
	q.model = paid
	q.payload.Set("model", paid)
	h.reshape(q)
	q.effortDone = false
	// The paid attempt is a new request: no account has been charged for it yet.
	q.at = attemptState{tried: map[string]struct{}{}, charged: map[string]bool{}}
	return true
}
