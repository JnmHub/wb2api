package openai

import (
	"strings"
	"time"

	"wb2api/internal/pool"
)

// viewModel is the model name the client sees: the one it asked for, even when
// an alias or a fallback chain step served the request.
func (q *request) viewModel() string {
	if q.clientModel != "" {
		return q.clientModel
	}
	return q.model
}

// modelBlockedForRealm reports whether no account in the realm can serve model
// right now. An account counts as able only when it is otherwise ready (not
// disabled, restricted, out of credits or cooling) and the model itself is not
// blocked on it: the per-model 429 ladder, a persisted 6004 window, or a lost
// entitlement.
//
// Concurrency saturation is deliberately not part of this: a busy pool is
// transient and must not burn a chain step.
func (h *Handler) modelBlockedForRealm(realmName, model string, now time.Time) bool {
	if h.Pool == nil {
		return false
	}
	seen := 0
	for _, a := range h.Pool.All() {
		rec := a.Record()
		if rec.Realm != realmName || !rec.IsEnabled() || rec.AccessToken == "" {
			continue
		}
		seen++
		switch a.State(now) {
		case pool.StatusReady, pool.StatusExpiring:
		default:
			continue // cannot serve anything right now
		}
		if !a.ModelBlocked(model, now) {
			return false
		}
	}
	return seen > 0
}

// modelBanned reports whether the fallback target is filtered out globally.
func (h *Handler) modelBanned(model string) bool {
	if h.Settings == nil {
		return false
	}
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return true
	}
	for _, banned := range h.Settings.BannedModels() {
		if strings.ToLower(strings.TrimSpace(banned)) == name {
			return true
		}
	}
	if sub := strings.ToLower(strings.TrimSpace(h.Settings.BannedSubstring())); sub != "" {
		if strings.Contains(name, sub) {
			return true
		}
	}
	return false
}

// advanceModelChain walks the configured fallback chain one step when the
// current model cannot be served by any account any more (option B: staged
// switching, not a per-account swap). true means the request was rewritten and
// the attempt loop should run again from the top.
func (h *Handler) advanceModelChain(q *request) bool {
	if h.Settings == nil {
		return false
	}
	if q.chain == nil {
		q.chain = h.Settings.ModelChain(q.model)
		q.chainPos = 0
	}
	if len(q.chain) < 2 {
		return false
	}
	now := time.Now()
	if !h.modelBlockedForRealm(q.realm, q.model, now) {
		return false
	}
	for q.chainPos+1 < len(q.chain) {
		q.chainPos++
		next := q.chain[q.chainPos]
		if next == "" || next == q.model {
			continue
		}
		if h.modelBanned(next) || h.modelBlockedForRealm(q.realm, next, now) {
			continue
		}
		if q.clientModel == "" {
			q.clientModel = q.model
		}
		h.logger().Warn("model exhausted on every account, moving down the fallback chain",
			"realm", q.realm, "model", q.model, "next", next)
		q.model = next
		q.payload.Set("model", next)
		h.reshape(q)
		q.effortDone = false
		// A new model is a new request: nothing has been charged for it yet.
		q.at = attemptState{tried: map[string]struct{}{}, charged: map[string]bool{}}
		return true
	}
	return false
}
