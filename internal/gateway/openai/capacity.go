package openai

import (
	"context"
	"net/http"
	"time"

	"wb2api/internal/pool"
)

// OccupancyLimiter supplies hints only; every dispatch must still atomically
// acquire both local and shared slots. Missing entries mean unknown, not full.
type OccupancyLimiter interface {
	Occupancy(ctx context.Context, accountIDs []string, fresh bool) (map[string]int, error)
}

// BoundedInflightLimiter separates single-probe I/O from lease lifetime. Unlike
// AcquireWait it does not wait for capacity after an atomic full response.
type BoundedInflightLimiter interface {
	AcquireWithin(ctx context.Context, accountID string, max int, member string, budget time.Duration) (func(), bool, error)
}

type admissionState struct {
	started time.Time
	counts  map[string]int
	loaded  bool
	fresh   bool
	refresh bool
	err     error
}

func (h *Handler) startAdmission(q *request) {
	if q.at.capacity == nil {
		q.at.capacity = &admissionState{started: time.Now()}
		if h.AccountWait > 0 {
			q.at.admissionDeadline = time.Now().Add(h.AccountWait)
		}
	}
}

func (h *Handler) recordAdmissionWait(q *request) {
	if s := q.at.capacity; s != nil && !s.started.IsZero() {
		addAdmissionWait(q.r.Context(), time.Since(s.started))
		s.started = time.Time{}
	}
}

func (h *Handler) endAdmission(q *request) {
	h.recordAdmissionWait(q)
	q.at.capacity = nil
	q.at.admissionDeadline = time.Time{}
	clear(q.at.full)
	q.at.waited, q.at.allFull = false, false
}

func (h *Handler) admissionBudget(q *request) time.Duration {
	budget := time.Second
	if !q.at.admissionDeadline.IsZero() {
		budget = minDur(budget, time.Until(q.at.admissionDeadline))
	}
	return budget
}

func (h *Handler) admissionFailed(q *request) bool {
	if q.at.capacity == nil || q.at.capacity.err == nil {
		return false
	}
	h.recordAdmissionWait(q)
	if q.r.Context().Err() == nil {
		q.httpErr(http.StatusServiceUnavailable, "shared account concurrency service unavailable; retry shortly", "server_error")
	}
	return true
}

func (h *Handler) loadOccupancy(q *request, candidates []*pool.Account, fresh bool) map[string]int {
	h.startAdmission(q)
	lim, ok := h.Limiter.(OccupancyLimiter)
	if !ok || h.AccountInflight <= 0 {
		return nil
	}
	s := q.at.capacity
	if s.loaded && !fresh && !s.refresh {
		return s.counts
	}
	budget := h.admissionBudget(q)
	if budget <= 0 {
		q.at.allFull = true
		return s.counts
	}
	ids := make([]string, 0, len(candidates))
	for _, a := range candidates {
		ids = append(ids, a.UID)
	}
	ctx, cancel := context.WithTimeout(q.r.Context(), budget)
	counts, err := lim.Occupancy(ctx, ids, fresh || s.refresh)
	cancel()
	if err != nil {
		// Hints never replace atomic admission. Explicit availability mode can
		// ignore a failed hint and leave the final gate to its normal policy.
		if !h.SharedLimitFailOpen && q.r.Context().Err() == nil && (q.at.admissionDeadline.IsZero() || time.Now().Before(q.at.admissionDeadline)) {
			s.err = err
		}
		s.counts, s.loaded, s.refresh = nil, true, false
		return nil
	}
	s.counts, s.loaded, s.refresh = counts, true, false
	if fresh {
		s.fresh = true
	}
	return counts
}

func (h *Handler) capacityCandidates(q *request) []*pool.Account {
	if pinned := PinnedFrom(q.r.Context()); pinned != nil {
		return []*pool.Account{pinned}
	}
	all := h.Pool.All()
	out := make([]*pool.Account, 0, len(all))
	now := time.Now()
	for _, a := range all {
		if a.Realm != q.realm || !a.HealthReady(q.model, now) {
			continue
		}
		if _, tried := q.at.tried[a.UID]; tried {
			continue
		}
		out = append(out, a)
	}
	return out
}

func (h *Handler) noteAccountFull(q *request, a *pool.Account) {
	h.startAdmission(q)
	delete(q.at.tried, a.UID)
	if q.at.full == nil {
		q.at.full = make(map[string]*pool.Account)
	}
	q.at.full[a.UID] = a
	if q.at.capacity.counts != nil {
		q.at.capacity.counts[a.UID] = h.AccountInflight
	}
	q.at.capacity.refresh = true
}

func (h *Handler) acquireAccount(q *request, a *pool.Account) (func(), bool, error) {
	h.startAdmission(q)
	budget := h.admissionBudget(q)
	if budget <= 0 {
		return nil, false, nil
	}
	var rel func()
	var ok bool
	var err error
	if lim, bounded := h.Limiter.(BoundedInflightLimiter); bounded {
		rel, ok, err = lim.AcquireWithin(q.r.Context(), a.UID, h.AccountInflight, q.turnID, budget)
	} else {
		// Compatibility: the legacy interface cannot separate admission I/O
		// from renewal lifetime. Keep its original request context and fence
		// late success rather than canceling an admitted lease prematurely.
		rel, ok, err = h.Limiter.Acquire(q.r.Context(), a.UID, h.AccountInflight, q.turnID)
	}
	if q.r.Context().Err() != nil || (!q.at.admissionDeadline.IsZero() && !time.Now().Before(q.at.admissionDeadline)) {
		if rel != nil {
			rel()
		}
		return nil, false, nil
	}
	return rel, ok, err
}
