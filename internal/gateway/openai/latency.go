package openai

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// requestLatency is shared by the Chat, Messages and Responses paths. Attempt
// identity prevents a timed-out first-output reader from recording into a retry.
// Zero durations mean unavailable, never an inferred zero-latency measurement.
type requestLatency struct {
	mu            sync.Mutex
	start         time.Time
	attempt       uint64
	dispatched    time.Time
	semantic      time.Time
	clientFlush   time.Time
	cancelled     time.Time
	admissionWait time.Duration
	keyWait       time.Duration
	emitted       bool
}

type latencyContextKey struct{}

// startLatency must run before admission/key waits. The returned cleanup only
// stops the cancellation observer; call it before normal request cancellation.
func startLatency(r *http.Request, start time.Time) (*http.Request, func()) {
	if latencyFrom(r.Context()) != nil {
		return r, func() {}
	}
	l := &requestLatency{start: start}
	ctx := context.WithValue(r.Context(), latencyContextKey{}, l)
	stop := context.AfterFunc(ctx, func() { noteCancellation(ctx) })
	return r.WithContext(ctx), func() { stop() }
}

func latencyFrom(ctx context.Context) *requestLatency {
	if ctx == nil {
		return nil
	}
	l, _ := ctx.Value(latencyContextKey{}).(*requestLatency)
	return l
}

// beginAttempt belongs immediately before an actual upstream dispatch, not
// before admission, account selection, retry backoff or web-tool execution.
// Responses follow-ups intentionally replace the upstream sample: its event
// describes the final actual attempt, without charging earlier rounds to it.
func beginAttempt(ctx context.Context) {
	if l := latencyFrom(ctx); l != nil {
		l.mu.Lock()
		l.attempt++
		l.dispatched, l.semantic = time.Now(), time.Time{}
		l.mu.Unlock()
	}
}

func addAdmissionWait(ctx context.Context, d time.Duration) {
	if l := latencyFrom(ctx); l != nil && d > 0 {
		l.mu.Lock()
		l.admissionWait += d
		l.mu.Unlock()
	}
}

func addKeyWait(ctx context.Context, d time.Duration) {
	if l := latencyFrom(ctx); l != nil && d > 0 {
		l.mu.Lock()
		l.keyWait += d
		l.mu.Unlock()
	}
}

// upstreamObserver is captured once per reader, before any read-ahead goroutine
// starts. Record at semantic read, before local pacing/filtering/conversion and
// before a staged prelude is replayed. Duplicate/replayed reads are harmless.
func upstreamObserver(ctx context.Context) func([]byte) {
	l := latencyFrom(ctx)
	if l == nil {
		return func([]byte) {}
	}
	l.mu.Lock()
	attempt := l.attempt
	complete := !l.semantic.IsZero() || l.dispatched.IsZero()
	l.mu.Unlock()
	var done atomic.Bool
	done.Store(complete)
	return func(line []byte) {
		// All subsequent chunks (including staged prelude replay) take only an
		// atomic load, not another JSON decode or shared lock.
		if done.Load() {
			return
		}
		// Capture read availability before decoding the semantic discriminator.
		now := time.Now()
		l.mu.Lock()
		obsolete := attempt != l.attempt || !l.semantic.IsZero()
		l.mu.Unlock()
		if obsolete {
			done.Store(true)
			return
		}
		if !semanticFrame(line) {
			return
		}
		l.mu.Lock()
		if attempt == l.attempt && !l.dispatched.IsZero() && l.semantic.IsZero() && !now.Before(l.dispatched) {
			l.semantic = now
		}
		l.mu.Unlock()
		done.Store(true)
	}
}

// noteClientFlush means a successful downstream first semantic Flush for streams,
// or the successful complete-document Flush for nonstream JSON. It does not
// assert receipt by the browser or arrival across the network.
func noteClientFlush(ctx context.Context) {
	if l := latencyFrom(ctx); l != nil {
		l.mu.Lock()
		if l.clientFlush.IsZero() {
			l.clientFlush = time.Now()
		}
		l.mu.Unlock()
	}
}

// noteCancellation records when cancellation is observed, including downstream
// write/deadline failure. Normal deferred upstream cleanup must not call it.
func noteCancellation(ctx context.Context) {
	if l := latencyFrom(ctx); l != nil {
		l.mu.Lock()
		if l.cancelled.IsZero() {
			l.cancelled = time.Now()
		}
		l.mu.Unlock()
	}
}

func positiveElapsed(start, end time.Time) time.Duration {
	if start.IsZero() || end.IsZero() || !end.After(start) {
		return 0
	}
	return end.Sub(start)
}

// applyLatency snapshots at relay finalization, not at deferred normal cleanup.
// A cancellation timestamp absent at this point stays unavailable rather than
// manufacturing a near-zero cancellation sample from ctx.Err().
func applyLatency(ctx context.Context, ev *UsageEvent) {
	if l := latencyFrom(ctx); l != nil {
		l.mu.Lock()
		defer l.mu.Unlock()
		ev.UpstreamTTFT = positiveElapsed(l.dispatched, l.semantic)
		ev.AdmissionWait, ev.KeyWait = l.admissionWait, l.keyWait
		ev.QueueWait = l.admissionWait + l.keyWait
		ev.ClientTTFT = positiveElapsed(l.start, l.clientFlush)
		ev.CancelLatency = positiveElapsed(l.cancelled, time.Now())
	}
}

// emitWithLatency prevents duplicate terminal usage events for a shared request.
// Requests without telemetry retain the legacy emission behavior (e.g. probes).
func (h *Handler) emitWithLatency(ctx context.Context, ev UsageEvent) {
	if l := latencyFrom(ctx); l != nil {
		l.mu.Lock()
		if l.emitted {
			l.mu.Unlock()
			return
		}
		l.emitted = true
		l.mu.Unlock()
	}
	applyLatency(ctx, &ev)
	h.emit(ev)
}
