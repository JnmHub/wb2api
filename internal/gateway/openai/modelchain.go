package openai

import (
	"strings"
	"time"

	"wb2api/internal/realm"
	"wb2api/internal/upstream/raccoon"
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
// right now. The test is HealthReady, the same predicate the picker uses before
// dispatching (minus local capacity): it already covers a disabled account, an
// upstream restriction, an exhausted balance, every cooldown, the manual
// per-model deny list, the per-model 429 ladder, a persisted 6004 window and a
// lost entitlement. Using anything narrower here makes the chain miss the
// common "the pool cannot serve this model" case and end in a 503 instead.
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
		if a.HealthReady(model, now) && (!realm.IsRaccoon(realmName) || raccoon.AllowsModel(rec, model)) {
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
		if len(q.chain) < 2 && realm.IsRaccoon(q.realm) && q.viewModel() != q.model {
			q.chain = h.Settings.ModelChain(q.viewModel())
		}
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
		if next == "" || next == q.model || h.modelBanned(next) {
			continue
		}
		if realm.IsRaccoon(q.realm) {
			next = raccoon.WireName(next)
			if next == q.model || h.modelBanned(next) {
				continue
			}
		}
		if h.modelBlockedForRealm(q.realm, next, now) {
			continue
		}
		if q.clientModel == "" {
			q.clientModel = q.model
		}
		h.logger().Warn("model exhausted on every account, moving down the fallback chain",
			"realm", q.realm, "model", q.model, "next", next)
		q.model = next
		q.payload.Set("model", next)
		if realm.IsRaccoon(q.realm) {
			q.shaped = q.payload
		} else {
			h.reshape(q)
		}
		q.effortDone = false
		// A new model is a new request: nothing has been charged for it yet.
		q.at = attemptState{tried: map[string]struct{}{}, charged: map[string]bool{}}
		return true
	}
	return false
}
