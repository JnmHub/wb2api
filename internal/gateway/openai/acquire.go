package openai

import (
	"time"

	"wb2api/internal/pool"
)

// waitForFullAccount is the bounded second phase of slot acquisition. Ordinary
// routes probe free accounts first; pinned routes can only wait for their owner.
// A successful result owns BOTH the local and shared slots, released together.
func (h *Handler) waitForFullAccount(q *request) (*pool.Account, func(), bool) {
	q.at.waited = true
	q.at.allFull = true
	now := time.Now()
	var best *pool.Account
	for _, a := range q.at.full {
		if a == nil || !a.HealthReady(q.model, now) {
			continue
		}
		if best == nil || h.betterAccount(a, best) {
			best = a
		}
	}
	if best == nil {
		return nil, nil, false
	}
	deadline := now.Add(h.AccountWait)
	delay := 10 * time.Millisecond
	var localRelease func()
	for {
		if q.r.Context().Err() != nil || !best.HealthReady(q.model, time.Now()) {
			return nil, nil, false
		}
		var ok bool
		localRelease, ok = best.TryAcquire(h.AccountInflight)
		if ok {
			break
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || !sleepCtx(q.r.Context(), minDur(delay, remaining)) {
			return nil, nil, false
		}
		delay = minDur(delay*2, 100*time.Millisecond)
	}
	sharedRelease := func() {}
	if h.Limiter != nil && h.AccountInflight > 0 {
		var rel func()
		var ok bool
		var err error
		if waiter, canWait := h.Limiter.(WaitingInflightLimiter); canWait {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				localRelease()
				return nil, nil, false
			}
			rel, ok, err = waiter.AcquireWait(q.r.Context(), best.UID, h.AccountInflight, q.turnID, remaining)
		} else {
			rel, ok, err = h.Limiter.Acquire(q.r.Context(), best.UID, h.AccountInflight, q.turnID)
		}
		if err != nil {
			localRelease()
			if q.r.Context().Err() == nil {
				h.logger().Warn("account wait failed", "account", short(best.UID), "err", err)
			}
			return nil, nil, false
		}
		if !ok {
			localRelease()
			return nil, nil, false
		}
		if rel != nil {
			sharedRelease = rel
		}
	}
	release := func() { localRelease(); sharedRelease() }
	if q.r.Context().Err() != nil || !best.HealthReady(q.model, time.Now()) {
		release()
		return nil, nil, false
	}
	q.at.tried[best.UID] = struct{}{}
	q.at.lastUID = best.UID
	q.at.allFull = false
	return best, release, true
}
