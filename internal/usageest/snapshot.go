package usageest

import (
	"context"
	"errors"
	"time"

	"wb2api/internal/pool"
)

var errInvalidated = errors.New("remaining estimate invalidated during refresh")

type refreshFlight struct {
	done chan struct{}
	err  error
}

// ScheduleSnapshot is the dispatch API: one atomic load, no I/O, locks, map
// copies or allocation. Cold start and invalidation return nil (advisory off).
// Start must be called before serving requests to maintain its freshness.
func (e *Estimator) ScheduleSnapshot() *pool.RemainingSnapshot { return e.schedule.Load() }

func (e *Estimator) refreshTimeout() time.Duration {
	if e.RefreshTimeout > 0 {
		return e.RefreshTimeout
	}
	return 5 * time.Second
}

// Invalidate prevents an in-flight old build from publishing. The advisory is
// immediately disabled until a fresh build; account eligibility stays in Pool.
func (e *Estimator) Invalidate() {
	e.mu.Lock()
	e.epoch++
	e.schedule.Store(nil)
	if e.wake != nil {
		select {
		case e.wake <- struct{}{}:
		default:
		}
	}
	e.mu.Unlock()
}

// Refresh shares one bounded backend read. A canceled waiter does not cancel
// the other waiters' work; Start's lifecycle cancellation does cancel its read.
func (e *Estimator) Refresh(ctx context.Context) error { return e.refresh(ctx, context.Background()) }
func (e *Estimator) refresh(ctx, life context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return context.Canceled
	}
	f := e.flight
	if f == nil {
		f = &refreshFlight{done: make(chan struct{})}
		e.flight = f
		epoch := e.epoch
		if e.flightCtx == nil {
			e.flightCtx, e.flightCancel = context.WithCancel(context.Background())
		}
		buildCtx, cancel := context.WithCancel(e.flightCtx)
		stopLife := context.AfterFunc(life, cancel)
		go func() {
			defer cancel()
			defer stopLife()
			e.build(buildCtx, f, epoch)
		}()
	}
	e.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-f.done:
		return f.err
	}
}

func (e *Estimator) build(life context.Context, f *refreshFlight, epoch uint64) {
	ctx, cancel := context.WithTimeout(life, e.refreshTimeout())
	defer cancel()
	now := e.now()
	b := &builder{est: e, now: now, series: map[string]*series{}, resets: map[string][]float64{}, budgets: map[string]budget{}}
	err := b.loadPoints(ctx)
	var p *Payload
	var weights []pool.RemainingWeight
	if err == nil {
		b.loadEvents()
		b.estimateBudgets()
		p = b.rows()
		p.WindowSeconds = int(windowSeconds)
		p.GeneratedAt = now.Unix()
		p.ETag = sum(p)
		for _, r := range p.Rows {
			if w := rowWeight(r); w > 1 {
				weights = append(weights, pool.RemainingWeight{Realm: r.Realm, Model: r.Model, UID: r.UID, Weight: w})
			}
		}
		err = ctx.Err()
	}
	var schedule *pool.RemainingSnapshot
	if err == nil {
		e.mu.Lock()
		revision := e.revision + 1 // only one build is admitted at a time
		e.mu.Unlock()
		schedule = pool.NewRemainingSnapshot(revision, weights)
		err = ctx.Err()
	}
	e.mu.Lock()
	if err == nil && epoch != e.epoch {
		err = errInvalidated
	}
	if err == nil {
		e.revision++
		e.cached, e.builtAt, e.cachedEpoch = p, now, epoch
		e.schedule.Store(schedule)
	}
	f.err = err
	e.flight = nil
	close(f.done)
	e.mu.Unlock()
	if err != nil && !errors.Is(err, errInvalidated) && !errors.Is(err, context.Canceled) {
		e.logger().Warn("remaining estimate: refresh failed; retaining prior payload", "err", err)
	}
}

// PayloadContext retains panel freshness semantics without holding a lock over
// I/O. On read failure it returns the last accurate payload plus the error,
// never replaces known estimates with a fabricated empty/zero-usage build.
func (e *Estimator) PayloadContext(ctx context.Context) (*Payload, error) {
	ctx, cancel := context.WithTimeout(ctx, e.refreshTimeout())
	defer cancel()
	for {
		e.mu.Lock()
		p := e.cached
		fresh := p != nil && e.cachedEpoch == e.epoch && e.now().Sub(e.builtAt) < e.ttl()
		e.mu.Unlock()
		if fresh {
			return p, nil
		}
		err := e.Refresh(ctx)
		if errors.Is(err, errInvalidated) {
			continue
		}
		if err != nil {
			e.mu.Lock()
			p = e.cached
			e.mu.Unlock()
			if p == nil {
				p = &Payload{WindowSeconds: int(windowSeconds), Rows: []Row{}, Models: []ModelTotal{}}
			}
			return p, err
		}
		e.mu.Lock()
		p = e.cached
		valid := e.cachedEpoch == e.epoch
		e.mu.Unlock()
		if valid {
			return p, nil
		}
	}
}

// Start initializes retained events before requests and starts a single owned
// background refresher. Cancellation stops future refreshes and cancels active
// lifecycle-owned SQL. Event persistence remains best effort and independently
// drains its bounded in-memory buffer. Configuration must not change afterward.
func (e *Estimator) Start(ctx context.Context) {
	e.startOnce.Do(func() {
		e.initEvents()
		e.mu.Lock()
		if e.stopped {
			e.mu.Unlock()
			return
		}
		ctx, e.lifeCancel = context.WithCancel(ctx)
		e.loopDone = make(chan struct{})
		done := e.loopDone
		e.wake = make(chan struct{}, 1)
		wake := e.wake
		e.mu.Unlock()
		go func() {
			defer close(done)
			interval := e.ttl()
			if interval < 100*time.Millisecond {
				interval = 100 * time.Millisecond
			}
			timer := time.NewTicker(interval)
			defer timer.Stop()
			for {
				if ctx.Err() != nil {
					return
				}
				_ = e.refresh(ctx, ctx)
				select {
				case <-ctx.Done():
					return
				case <-timer.C:
				case <-wake:
				}
			}
		}()
	})
}

// Stop prevents new refresh flights, cancels Start-owned SQL, and joins both
// the loop and any panel-owned in-flight build. Call only after request
// producers are quiescent, then FlushEvents before closing the usage database.
// A deadline reports incomplete joining; callers must keep storage alive.
func (e *Estimator) Stop(ctx context.Context) error {
	e.mu.Lock()
	e.stopped = true
	if e.lifeCancel != nil {
		e.lifeCancel()
	}
	if e.flightCancel != nil {
		e.flightCancel()
	}
	loop := e.loopDone
	var flight <-chan struct{}
	if e.flight != nil {
		flight = e.flight.done
	}
	e.mu.Unlock()
	for _, done := range []<-chan struct{}{loop, flight} {
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func rowWeight(r Row) int {
	if r.Cooling || r.Budget == nil || r.Remaining == nil || *r.Budget <= 0 || *r.Remaining <= 0 {
		return 1
	}
	ratio := float64(*r.Remaining) / float64(*r.Budget)
	switch {
	case ratio <= bandVeryLow:
		return 4
	case ratio <= bandLow:
		return 3
	case ratio <= bandMedium:
		return 2
	default:
		return 1
	}
}
