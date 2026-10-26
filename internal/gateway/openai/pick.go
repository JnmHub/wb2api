package openai

import "wb2api/internal/pool"

// pickAccount applies a single batch of advisory capacity exclusions before the
// existing pool picker, leaving its sticky/manual/expiry/remaining/TopK policy
// intact. A saturated pool therefore requires O(N) hint work, not N full scans
// and N sequential Redis atomic probes. Feedback escape remains a preference.
func (h *Handler) pickAccount(q *request) *pool.Account {
	h.startAdmission(q)
	candidates := h.capacityCandidates(q)
	counts := h.loadOccupancy(q, candidates, false)
	for pass := 0; pass < 2; pass++ {
		if q.r.Context().Err() != nil || q.at.capacity.err != nil {
			return nil
		}
		if !q.at.admissionDeadline.IsZero() && h.admissionBudget(q) <= 0 {
			q.at.allFull = true
			return nil
		}
		base := make(map[string]struct{}, len(q.at.tried)+len(q.at.full)+len(candidates))
		for uid := range q.at.tried {
			base[uid] = struct{}{}
		}
		for _, a := range candidates {
			n, known := counts[a.UID]
			_, denied := q.at.full[a.UID]
			full := a.AtCapacity(h.AccountInflight) || (known && h.AccountInflight > 0 && n >= h.AccountInflight)
			// Unknown shared occupancy must not erase a concrete atomic denial.
			if full || (denied && !known) {
				base[a.UID] = struct{}{}
				if q.at.full == nil {
					q.at.full = make(map[string]*pool.Account)
				}
				q.at.full[a.UID] = a
			} else {
				delete(q.at.full, a.UID)
			}
		}
		exclude := base
		if len(q.at.avoid) > 0 {
			exclude = unionExclude(base, q.at.avoid)
		}
		if len(q.keyDenied) > 0 {
			exclude = unionExclude(exclude, q.keyDenied)
		}
		a := h.Pool.SelectForSessionBestContext(q.r.Context(), q.realm, q.sessionKey, exclude, q.model, q.page, h.betterAccount)
		if a == nil && len(q.at.avoid) > 0 {
			alt := base
			if len(q.keyDenied) > 0 {
				alt = unionExclude(base, q.keyDenied)
			}
			a = h.Pool.SelectForSessionBestContext(q.r.Context(), q.realm, q.sessionKey, alt, q.model, q.page, h.betterAccount)
		}
		if a != nil {
			return a
		}
		// A short-lived cached full hint cannot permanently exclude an account
		// released since the previous request, even with AccountWait == 0.
		if _, ok := h.Limiter.(OccupancyLimiter); !ok || q.at.capacity.fresh || pass != 0 {
			break
		}
		counts = h.loadOccupancy(q, candidates, true)
	}
	if len(q.at.full) > 0 {
		q.at.allFull = true
	}
	return nil
}
