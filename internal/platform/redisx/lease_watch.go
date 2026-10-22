package redisx

import (
	"context"
	"sync"
	"time"
)

// leaseWatch fences the owner before its last confirmed server lease can expire,
// independently of a heartbeat stuck in network I/O. Only successful renewal
// acknowledgments extend the local deadline. An ambiguous renewal never does.
type leaseWatch struct {
	mu        sync.Mutex
	ctx       context.Context
	deadline  time.Time
	timer     *time.Timer
	stopped   bool
	notifying chan struct{}
}

func newLeaseWatch(ctx context.Context, confirmed time.Time, ttl time.Duration) *leaseWatch {
	w := &leaseWatch{ctx: ctx}
	w.confirm(confirmed, ttl)
	return w
}

// loseLocked irrevocably fences this watch. Invoke the callback outside mu so
// application cancellation callbacks cannot deadlock the timer state lock.
func (w *leaseWatch) loseLocked() chan struct{} {
	w.stopped = true
	if w.timer != nil {
		w.timer.Stop()
	}
	w.notifying = make(chan struct{})
	return w.notifying
}

func (w *leaseWatch) notify(done chan struct{}) {
	defer close(done)
	loseLease(w.ctx)
}

func (w *leaseWatch) confirm(at time.Time, ttl time.Duration) {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	// The old deadline may have passed while its timer callback was delayed
	// by scheduling. A late ACK must never revive an already unsafe holder.
	now := time.Now()
	next := at.Add(ttl - min(ttl/10, time.Second))
	if (!w.deadline.IsZero() && !now.Before(w.deadline)) || !now.Before(next) {
		done := w.loseLocked()
		w.mu.Unlock()
		w.notify(done)
		return
	}
	w.deadline = next
	wait := max(time.Until(w.deadline), time.Nanosecond)
	if w.timer == nil {
		w.timer = time.AfterFunc(wait, w.expire)
	} else {
		w.timer.Reset(wait)
	}
	w.mu.Unlock()
}

// lose handles an explicit server rejection through the same once-only path as
// timeout, so an overlapping rejection and timer cannot notify twice.
func (w *leaseWatch) lose() {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	done := w.loseLocked()
	w.mu.Unlock()
	w.notify(done)
}

func (w *leaseWatch) expire() {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	if left := time.Until(w.deadline); left > 0 {
		w.timer.Reset(left)
		w.mu.Unlock()
		return
	}
	done := w.loseLocked()
	w.mu.Unlock()
	w.notify(done)
}

func (w *leaseWatch) stop() {
	w.mu.Lock()
	w.stopped = true
	if w.timer != nil {
		w.timer.Stop()
	}
	done := w.notifying
	w.mu.Unlock()
	// If expiry won the race, Release must wait for its cancellation callback
	// rather than returning and allowing that callback to fire after release.
	if done != nil {
		<-done
	}
}
