package openai

import (
	"sort"
	"sync"
	"time"

	"wb2api/internal/pool"
)

// waitForFullAccount is the bounded second phase of slot acquisition. Ordinary
// routes retry the eligible full set, rather than waiting exclusively for its
// best member. Pinned routes can only wait for their owner. A successful result
// owns BOTH the local and shared slots, released together.
func (h *Handler) waitForFullAccount(q *request) (*pool.Account, func(), bool) {
	q.at.waited = true
	q.at.allFull = true
	ctx := q.r.Context()
	if ctx.Err() != nil || h.AccountWait <= 0 {
		return nil, nil, false
	}
	h.startAdmission(q)
	deadline := q.at.admissionDeadline
	pinned := PinnedFrom(ctx)
	type candidate struct {
		account *pool.Account
		score   int64
	}
	candidates := make([]candidate, 0, len(q.at.full))
	for _, a := range q.at.full {
		if a == nil || (pinned != nil && a.UID != pinned.UID) {
			continue
		}
		candidates = append(candidates, candidate{a, h.AccountScore(a, "")})
	}
	// Snapshot rank only sets preference. Capacity and health are rechecked
	// every round and after admission. Stable tie-breaking also aids testing.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score < candidates[j].score
		}
		return candidates[i].account.UID < candidates[j].account.UID
	})
	multiple := len(candidates) > 1
	delay := 10 * time.Millisecond
	failed := make(map[string]bool)
	for {
		roundStart := time.Now()
		accounts := make([]*pool.Account, 0, len(candidates))
		for _, c := range candidates {
			accounts = append(accounts, c.account)
		}
		counts := h.loadOccupancy(q, accounts, true)
		if q.at.capacity.err != nil {
			return nil, nil, false
		}
		eligible := false
		for i, c := range candidates {
			a := c.account
			if ctx.Err() != nil || time.Until(deadline) <= 0 {
				return nil, nil, false
			}
			if failed[a.UID] || !a.HealthReady(q.model, time.Now()) {
				continue
			}
			eligible = true
			if n, known := counts[a.UID]; known && h.AccountInflight > 0 && n >= h.AccountInflight {
				continue
			}
			localRelease, ok := a.TryAcquire(h.AccountInflight)
			if !ok {
				continue
			}
			sharedRelease := func() {}
			if h.Limiter != nil && h.AccountInflight > 0 {
				remaining := time.Until(deadline)
				if remaining <= 0 || ctx.Err() != nil {
					localRelease()
					return nil, nil, false
				}
				var rel func()
				var err error
				if bounded, canBound := h.Limiter.(BoundedInflightLimiter); canBound {
					// Hints already found potential space. Do one atomic probe,
					// not a per-account queue that serially stalls alternatives.
					rel, ok, err = bounded.AcquireWithin(ctx, a.UID, h.AccountInflight, q.turnID, minDur(remaining, 10*time.Millisecond))
				} else if waiter, canWait := h.Limiter.(WaitingInflightLimiter); canWait {
					budget := remaining
					if multiple {
						// Leave the rest of this round a share even when AccountWait
						// itself is shorter than one normal probe quantum.
						budget = minDur(budget/time.Duration(len(candidates)-i), 10*time.Millisecond)
						if budget <= 0 {
							localRelease()
							return nil, nil, false
						}
					}
					// maxWait bounds admission only. Passing a short-lived probe
					// context here would stop successful Redis lease renewal.
					rel, ok, err = waiter.AcquireWait(ctx, a.UID, h.AccountInflight, q.turnID, budget)
				} else {
					// Legacy Acquire has no separate admission budget. Preserve
					// its request-scoped lease context; a blocking implementation
					// can overrun AccountWait (checked again on return). It must
					// implement AcquireWait to bound I/O independently of lease life.
					rel, ok, err = h.Limiter.Acquire(ctx, a.UID, h.AccountInflight, q.turnID)
				}
				if err != nil && h.SharedLimitFailOpen && ctx.Err() == nil {
					// Explicit availability policy, never a silent downgrade.
					err, ok = nil, true
				}
				if err != nil || !ok {
					if rel != nil {
						rel()
					}
					localRelease()
					if err != nil {
						if ctx.Err() == nil && time.Now().Before(deadline) {
							q.at.capacity.err = err
						}
						if !h.SharedLimitFailOpen {
							return nil, nil, false
						}
						failed[a.UID] = true
						if ctx.Err() == nil {
							h.logger().Warn("account wait failed", "account", short(a.UID), "err", err)
						}
					}
					if !multiple {
						// AcquireWait already spent the owner's whole budget; an
						// atomic-only probe must refresh and retry until deadline.
						if _, atomicOnly := h.Limiter.(BoundedInflightLimiter); !atomicOnly {
							return nil, nil, false
						}
					}
					continue
				}
				if rel != nil {
					sharedRelease = rel
				}
			}
			var once sync.Once
			release := func() { once.Do(func() { localRelease(); sharedRelease() }) }
			if ctx.Err() != nil || time.Until(deadline) <= 0 {
				release()
				return nil, nil, false
			}
			if !a.HealthReady(q.model, time.Now()) {
				release()
				continue
			}
			if q.at.tried == nil {
				q.at.tried = make(map[string]struct{})
			}
			q.at.tried[a.UID] = struct{}{}
			q.at.lastUID = a.UID
			q.at.allFull = false
			return a, release, true
		}
		if !eligible {
			return nil, nil, false
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, nil, false
		}
		// Fast-denying fakes/legacy limiters and local-only contention need a
		// sleep; real shared waits already spent their round's budget in I/O.
		pause := delay - time.Since(roundStart)
		if pause > 0 && !sleepCtx(ctx, minDur(pause, remaining)) {
			return nil, nil, false
		}
		if !multiple {
			delay = minDur(delay*2, 100*time.Millisecond)
		}
	}
}
