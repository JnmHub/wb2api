package openai

import (
	"context"
	"time"

	"wb2api/internal/pool"
)

// ModelWindowStore is the shared per-model cooldown view. Redis implements it;
// nil means this replica only knows the windows it learned itself.
type ModelWindowStore interface {
	Mark(ctx context.Context, realmName, model, uid string, until time.Time) error
	Clear(ctx context.Context, realmName, model, uid string) error
	Blocked(ctx context.Context, realmName, model string, uids []string) (map[string]time.Time, error)
}

const (
	// sharedWindowTTL is how long an answer from the shared store is reused
	// before the next request asks again. Short enough that a cooldown learned
	// by replica A stops replica B quickly, long enough that a burst of
	// requests costs one pipeline.
	sharedWindowTTL = time.Second
	// sharedWindowTimeout bounds the whole read so a slow Redis only delays a
	// request by this much; the local view keeps working.
	sharedWindowTimeout = 900 * time.Millisecond
	// sharedWindowWriteTimeout bounds the fire-and-forget publish.
	sharedWindowWriteTimeout = 2 * time.Second
)

type windowEntry struct {
	at      time.Time
	blocked map[string]time.Time
}

// refreshSharedWindows pulls the windows other replicas learned for
// realm+model and adopts them locally, so a model that was refused elsewhere is
// skipped here too. Errors are ignored on purpose: a missing shared view must
// degrade to the per-replica behaviour, not fail the request.
func (h *Handler) refreshSharedWindows(ctx context.Context, realmName, model string, accs []*pool.Account, force bool) {
	if h.ModelWindows == nil || len(accs) == 0 || model == "" {
		return
	}
	key := realmName + "/" + model
	now := time.Now()
	if !force {
		h.winMu.Lock()
		if e, ok := h.winCache[key]; ok && now.Sub(e.at) < sharedWindowTTL {
			blocked := e.blocked
			h.winMu.Unlock()
			h.adoptWindows(model, blocked)
			return
		}
		h.winMu.Unlock()
	}
	uids := make([]string, 0, len(accs))
	for _, a := range accs {
		if uid := a.Record().UID; uid != "" {
			uids = append(uids, uid)
		}
	}
	if len(uids) == 0 {
		return
	}
	opCtx, cancel := context.WithTimeout(ctx, sharedWindowTimeout)
	blocked, err := h.ModelWindows.Blocked(opCtx, realmName, model, uids)
	cancel()
	if err != nil {
		// No negative caching of errors: the next request retries, but this one
		// proceeds on the local view.
		return
	}
	h.winMu.Lock()
	if h.winCache == nil {
		h.winCache = map[string]windowEntry{}
	}
	h.winCache[key] = windowEntry{at: now, blocked: blocked}
	h.winMu.Unlock()
	h.adoptWindows(model, blocked)
}

// adoptWindows turns shared windows into local cooldowns so the picker, the
// health re-check and the panel all agree without another Redis round trip.
func (h *Handler) adoptWindows(model string, blocked map[string]time.Time) {
	if len(blocked) == 0 {
		return
	}
	now := time.Now()
	for uid, until := range blocked {
		if !until.After(now) {
			continue
		}
		acc := h.Pool.Get(uid)
		if acc == nil {
			continue
		}
		if local := acc.ModelCoolUntil(model, now); local.After(until) {
			continue // the local view already runs longer, keep it
		}
		acc.CoolDownModelWhy(model, until, "shared")
	}
}

// publishWindowAllowed rate limits one shared-window write per account and
// model: cooling marks at most every two seconds, clears at most every thirty
// seconds. It keeps the write path off the request latency budget under a
// 429 storm while still sharing what this replica learned.
func (h *Handler) publishWindowAllowed(uid, model string, succeeded bool, now time.Time) bool {
	min := 2 * time.Second
	if succeeded {
		min = 30 * time.Second
	}
	key := uid + "\x00" + model

	h.winPubMu.Lock()
	defer h.winPubMu.Unlock()
	if h.winPubAt == nil {
		h.winPubAt = make(map[string]time.Time)
	}
	if last, ok := h.winPubAt[key]; ok && now.Sub(last) < min {
		return false
	}
	h.winPubAt[key] = now
	return true
}

// publishModelWindows shares the windows this replica just learned for the
// model it tried, so the other replica stops dispatching there too.
func (h *Handler) publishModelWindows(acc *pool.Account, model string, succeeded bool) {
	if h.ModelWindows == nil || acc == nil || model == "" {
		return
	}
	rec := acc.Record()
	if rec.UID == "" {
		return
	}
	now := time.Now()
	var until time.Time
	for _, c := range acc.ModelCooldowns(now) {
		if c.Model == model && c.Until.After(until) {
			until = c.Until
		}
	}
	if !succeeded && until.IsZero() {
		// Nothing was learned here: the attempt failed for an account-wide
		// reason, or the model window already expired.
		return
	}
	if !h.publishWindowAllowed(rec.UID, model, succeeded, now) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), sharedWindowWriteTimeout)
		defer cancel()
		if succeeded {
			_ = h.ModelWindows.Clear(ctx, rec.Realm, model, rec.UID)
			return
		}
		_ = h.ModelWindows.Mark(ctx, rec.Realm, model, rec.UID, until)
	}()
}
