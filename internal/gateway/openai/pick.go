package openai

import "wb2api/internal/pool"

// pickAccount treats feedback-based escape as a preference, not a health
// exclusion. If no alternative can serve, retry the still-healthy warm account.
// Truly tried accounts remain excluded on both passes.
func (h *Handler) pickAccount(q *request) *pool.Account {
	exclude := q.at.tried
	if len(q.at.avoid) > 0 {
		exclude = unionExclude(q.at.tried, q.at.avoid)
	}
	a := h.Pool.PickForSessionBest(q.realm, q.sessionKey, exclude, q.model, q.page, h.betterAccount)
	if a == nil && len(q.at.avoid) > 0 {
		fallbackExclude := make(map[string]struct{}, len(q.at.tried)+len(q.at.full))
		for uid := range q.at.tried {
			fallbackExclude[uid] = struct{}{}
		}
		for uid := range q.at.full {
			fallbackExclude[uid] = struct{}{}
		}
		a = h.Pool.PickForSessionBest(q.realm, q.sessionKey, fallbackExclude, q.model, q.page, h.betterAccount)
	}
	return a
}
