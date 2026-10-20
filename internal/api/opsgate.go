package api

import (
	"context"
	"sync"
	"time"
)

// OpsGate limits the number of concurrent vendor-facing account operations
// (credit refresh, check-in, daily chat, ...). It is mutable at runtime so the
// settings page can trade batch speed for a gentler upstream request rate.
type OpsGate struct {
	mu       sync.Mutex
	active   int
	limit    int
	interval time.Duration
	changed  chan struct{}
}

// NewOpsGate builds a gate; values are clamped to a safe range.
func NewOpsGate(limit int, interval time.Duration) *OpsGate {
	g := &OpsGate{changed: make(chan struct{})}
	g.Configure(limit, interval)
	return g
}

// Configure applies a new limit and inter-item pause to future acquisitions.
func (g *OpsGate) Configure(limit int, interval time.Duration) {
	if limit < 1 {
		limit = 1
	}
	if limit > 16 {
		limit = 16
	}
	if interval < 0 {
		interval = 0
	}
	g.mu.Lock()
	g.limit, g.interval = limit, interval
	close(g.changed)
	g.changed = make(chan struct{})
	g.mu.Unlock()
}

// Do runs f with a concurrency slot and applies the configured pause afterwards.
func (g *OpsGate) Do(ctx context.Context, f func() error) error {
	if g == nil {
		return f()
	}
	if !g.acquire(ctx) {
		return ctx.Err()
	}
	defer g.release()
	err := f()
	g.mu.Lock()
	interval := g.interval
	g.mu.Unlock()
	if interval > 0 {
		select {
		case <-time.After(interval):
		case <-ctx.Done():
		}
	}
	return err
}

func (g *OpsGate) acquire(ctx context.Context) bool {
	for {
		g.mu.Lock()
		if g.active < g.limit {
			g.active++
			g.mu.Unlock()
			return true
		}
		ch := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-ch:
		}
	}
}

func (g *OpsGate) release() {
	g.mu.Lock()
	if g.active > 0 {
		g.active--
	}
	close(g.changed)
	g.changed = make(chan struct{})
	g.mu.Unlock()
}
