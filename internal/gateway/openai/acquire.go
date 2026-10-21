package openai

import (
	"time"

	"wb2api/internal/pool"
)

// waitForFullAccount is the bounded second phase of slot acquisition. The
// caller has already probed every ready account without blocking, so a full
// account may now be worth waiting for. We queue on the best-ranked candidate
// only; a timed-out wait ends the request with a retryable 429 instead of
// sleeping once per full account.
func (h *Handler) waitForFullAccount(q *request) (*pool.Account, func(), bool) {
	q.at.waited = true
	q.at.allFull = true
	if h.Limiter == nil || h.AccountInflight <= 0 {
		return nil, nil, false
	}
	waiter, ok := h.Limiter.(WaitingInflightLimiter)
	if !ok {
		return nil, nil, false
	}
	now := time.Now()
	var best *pool.Account
	for _, a := range q.at.full {
		if a == nil || !a.Ready(q.model, now) {
			continue
		}
		if best == nil || h.betterAccount(a, best) {
			best = a
		}
	}
	if best == nil {
		return nil, nil, false
	}
	rel, ok, err := waiter.AcquireWait(q.r.Context(), best.UID, h.AccountInflight, q.turnID, h.AccountWait)
	if err != nil {
		if q.r.Context().Err() != nil {
			return nil, nil, false
		}
		h.logger().Warn("account wait failed", "account", short(best.UID), "err", err)
		return nil, nil, false
	}
	if !ok {
		return nil, nil, false
	}
	q.at.tried[best.UID] = struct{}{}
	q.at.lastUID = best.UID
	q.at.allFull = false
	return best, rel, true
}
