package openai

import (
	"context"
	"sync"
	"time"
)

// KeyLimiter is the optional Redis-backed API-key gate. When absent, each
// process uses keyGate; when present, the limit is shared by every replica.
type KeyLimiter interface {
	AcquireKeyWait(ctx context.Context, keyID string, max int, member string, maxWait time.Duration) (func(), bool, error)
}

// keyGate is a stable local, per-process counter. Handler.keyMu protects its
// fields. A limit change never replaces the counter or forgets older holders.
// changed is a broadcast notification, not a semaphore and not a polling timer.
type keyGate struct {
	active  int
	limit   int
	changed chan struct{}
	waiters int
}

func (g *keyGate) notifyLocked() {
	close(g.changed)
	g.changed = make(chan struct{})
}

// acquireKey reserves one slot for keyID, waiting at most AccountWait when busy.
// The local release is idempotent and must be called when the request finishes.
// A nonpositive local limit disables admission restrictions, but still counts
// holders so re-enabling a limit cannot overlook requests admitted while off.
// Shared Redis holders are not represented in the fallback's local counter.
func (h *Handler) acquireKey(ctx context.Context, keyID, member string, limit int) (func(), bool) {
	rel, ok, _ := h.acquireKeyResult(ctx, keyID, member, limit)
	return rel, ok
}

func (h *Handler) acquireKeyResult(ctx context.Context, keyID, member string, limit int) (func(), bool, error) {
	if ctx.Err() != nil {
		return nil, false, nil
	}
	if keyID == "" {
		return func() {}, true, nil
	}
	wait := h.AccountWait
	if wait <= 0 {
		wait = 2 * time.Second
	}
	if limit > 0 && h.KeyLimiter != nil {
		rel, ok, err := h.KeyLimiter.AcquireKeyWait(ctx, keyID, limit, member, wait)
		if err == nil {
			return rel, ok, nil
		}
		if ctx.Err() != nil {
			return nil, false, nil
		}
		if !h.SharedLimitFailOpen {
			return nil, false, err
		}
		h.logger().Warn("shared key limiter unavailable; explicitly configured local fallback", "key", keyID, "err", err)
	}
	h.keyMu.Lock()
	if h.keyGates == nil {
		h.keyGates = map[string]*keyGate{}
	}
	g := h.keyGates[keyID]
	if g == nil {
		g = &keyGate{limit: limit, changed: make(chan struct{})}
		h.keyGates[keyID] = g
	} else if g.limit != limit {
		g.limit = limit
		g.notifyLocked()
	}

	waitCtx := ctx
	var cancel context.CancelFunc
	for {
		if waitCtx.Err() != nil {
			h.keyMu.Unlock()
			return nil, false, nil
		}
		if g.limit <= 0 || g.active < g.limit {
			g.active++
			h.keyMu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					h.keyMu.Lock()
					g.active--
					g.notifyLocked()
					h.keyMu.Unlock()
				})
			}, true, nil
		}
		if cancel == nil {
			waitCtx, cancel = context.WithTimeout(ctx, wait)
			defer cancel() // created once, only when admission actually waits
		}
		changed := g.changed
		g.waiters++
		h.keyMu.Unlock()
		select {
		case <-waitCtx.Done():
		case <-changed:
		}
		h.keyMu.Lock()
		g.waiters--
		// Read the current gate limit, not this request's old limit argument.
		// Otherwise a woken waiter could undo a concurrent configuration change.
	}
}
