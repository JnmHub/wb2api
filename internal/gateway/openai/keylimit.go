package openai

import (
	"context"
	"time"
)

// KeyLimiter is the optional Redis-backed API-key gate. When absent, each
// process uses keyGate; when present, the limit is shared by every replica.
type KeyLimiter interface {
	AcquireKeyWait(ctx context.Context, keyID string, max int, member string, maxWait time.Duration) (func(), bool, error)
}

// keyGate is a local, per-process API-key concurrency gate. It is the fallback
// when no shared Redis counter is configured; the limit still protects the
// upstream from one key taking every account slot.
type keyGate struct {
	ch    chan struct{}
	limit int
}

// acquireKey reserves one slot for keyID. It waits briefly for a busy key
// instead of failing the request immediately, then gives up. The returned
// function must be called exactly once when the request finishes.
func (h *Handler) acquireKey(ctx context.Context, keyID, member string, limit int) (func(), bool) {
	if keyID == "" || limit <= 0 {
		return func() {}, true
	}
	wait := h.AccountWait
	if wait <= 0 {
		wait = 2 * time.Second
	}
	if h.KeyLimiter != nil {
		rel, ok, err := h.KeyLimiter.AcquireKeyWait(ctx, keyID, limit, member, wait)
		if err == nil {
			return rel, ok
		}
		if ctx.Err() != nil {
			return nil, false
		}
		h.logger().Warn("shared key limiter unavailable; using local gate", "key", keyID, "err", err)
	}
	h.keyMu.Lock()
	if h.keyGates == nil {
		h.keyGates = map[string]*keyGate{}
	}
	g := h.keyGates[keyID]
	if g == nil || g.limit != limit {
		g = &keyGate{ch: make(chan struct{}, limit), limit: limit}
		h.keyGates[keyID] = g
	}
	h.keyMu.Unlock()

	release := func() { <-g.ch }
	select {
	case g.ch <- struct{}{}:
		return release, true
	default:
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, false
		case <-deadline.C:
			return nil, false
		case <-tick.C:
			select {
			case g.ch <- struct{}{}:
				return release, true
			default:
			}
		}
	}
}
