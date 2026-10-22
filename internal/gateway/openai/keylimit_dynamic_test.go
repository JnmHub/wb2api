package openai

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type dynamicKeyResult struct {
	release func()
	ok      bool
}

func startDynamicKeyAcquire(h *Handler, ctx context.Context, limit int) <-chan dynamicKeyResult {
	done := make(chan dynamicKeyResult, 1)
	go func() {
		release, ok := h.acquireKey(ctx, "key", "request", limit)
		done <- dynamicKeyResult{release, ok}
	}()
	return done
}

func receiveDynamicKey(t *testing.T, done <-chan dynamicKeyResult) dynamicKeyResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for key acquisition")
		return dynamicKeyResult{}
	}
}

// Observe registration, rather than sleeping and assuming a goroutine reached
// the gate. Polling here is test synchronization only; the gate uses broadcasts.
func waitDynamicKeyState(t *testing.T, h *Handler, active, limit, waiters int) *keyGate {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		h.keyMu.Lock()
		g := h.keyGates["key"]
		match := g != nil && g.active == active && g.limit == limit && g.waiters == waiters
		h.keyMu.Unlock()
		if match {
			return g
		}
		select {
		case <-deadline.C:
			t.Fatalf("did not reach active=%d limit=%d waiters=%d", active, limit, waiters)
		case <-tick.C:
		}
	}
}

func TestKeyDynamicReductionPreservesOlderHolders(t *testing.T) {
	h := &Handler{AccountWait: time.Second}
	var releases []func()
	for i := 0; i < 3; i++ {
		release, ok := h.acquireKey(context.Background(), "key", fmt.Sprint(i), 3)
		if !ok {
			t.Fatal("initial holder rejected")
		}
		releases = append(releases, release)
	}
	original := h.keyGates["key"]
	waiting := startDynamicKeyAcquire(h, context.Background(), 1)
	if current := waitDynamicKeyState(t, h, 3, 1, 1); current != original {
		t.Fatal("limit change replaced gate/counter")
	}
	releases[0]()
	waitDynamicKeyState(t, h, 2, 1, 1)
	releases[1]()
	waitDynamicKeyState(t, h, 1, 1, 1)
	select {
	case <-waiting:
		t.Fatal("reduction admitted while active >= new cap")
	default:
	}
	releases[2]()
	result := receiveDynamicKey(t, waiting)
	if !result.ok {
		t.Fatal("old holder release did not wake waiter")
	}
	result.release()
	waitDynamicKeyState(t, h, 0, 1, 0)
}

func TestKeyDynamicIncreaseWakesExistingWaiters(t *testing.T) {
	h := &Handler{AccountWait: time.Second}
	first, ok := h.acquireKey(context.Background(), "key", "first", 1)
	if !ok {
		t.Fatal("initial holder rejected")
	}
	waiting := startDynamicKeyAcquire(h, context.Background(), 1)
	original := waitDynamicKeyState(t, h, 1, 1, 1)
	third, ok := h.acquireKey(context.Background(), "key", "increase", 3)
	if !ok {
		t.Fatal("increase did not create capacity")
	}
	result := receiveDynamicKey(t, waiting)
	if !result.ok {
		t.Fatal("increase did not notify existing waiter")
	}
	if current := waitDynamicKeyState(t, h, 3, 3, 0); current != original {
		t.Fatal("increase replaced gate")
	}
	// The waiter started with limit=1; it must not reset the current limit.
	first()
	third()
	result.release()
	waitDynamicKeyState(t, h, 0, 3, 0)
}

func TestKeyDynamicDisableAndReenableCountsLocalHolders(t *testing.T) {
	h := &Handler{AccountWait: time.Second}
	first, ok := h.acquireKey(context.Background(), "key", "first", 1)
	if !ok {
		t.Fatal("initial holder rejected")
	}
	waiting := startDynamicKeyAcquire(h, context.Background(), 1)
	waitDynamicKeyState(t, h, 1, 1, 1)
	unlimited, ok := h.acquireKey(context.Background(), "key", "disabled", 0)
	if !ok {
		t.Fatal("disabled gate did not admit")
	}
	oldWaiter := receiveDynamicKey(t, waiting)
	if !oldWaiter.ok {
		t.Fatal("disabling did not wake existing waiter")
	}
	waitDynamicKeyState(t, h, 3, 0, 0)
	reenabled := startDynamicKeyAcquire(h, context.Background(), 1)
	waitDynamicKeyState(t, h, 3, 1, 1)
	first()
	unlimited()
	waitDynamicKeyState(t, h, 1, 1, 1)
	oldWaiter.release()
	result := receiveDynamicKey(t, reenabled)
	if !result.ok {
		t.Fatal("re-enabled gate did not admit after all old holders left")
	}
	result.release()
	waitDynamicKeyState(t, h, 0, 1, 0)
}

func TestKeyDynamicReleaseIsIdempotentAndConcurrentSafe(t *testing.T) {
	h := &Handler{}
	release, ok := h.acquireKey(context.Background(), "key", "first", 1)
	if !ok {
		t.Fatal("initial holder rejected")
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); release() }()
	}
	wg.Wait()
	waitDynamicKeyState(t, h, 0, 1, 0)
	next, ok := h.acquireKey(context.Background(), "key", "next", 1)
	if !ok {
		t.Fatal("gate not reusable after duplicate release")
	}
	release() // must not release the newer request
	waitDynamicKeyState(t, h, 1, 1, 0)
	next()
}

func TestKeyDynamicCancellation(t *testing.T) {
	for _, limit := range []int{-1, 0, 1} {
		t.Run(fmt.Sprint("preCanceledLimit=", limit), func(t *testing.T) {
			h := &Handler{}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if release, ok := h.acquireKey(ctx, "key", "canceled", limit); ok || release != nil {
				t.Fatal("pre-canceled request admitted")
			}
			if len(h.keyGates) != 0 {
				t.Fatal("pre-canceled request created gate")
			}
		})
	}
	h := &Handler{AccountWait: time.Second}
	release, _ := h.acquireKey(context.Background(), "key", "holder", 1)
	ctx, cancel := context.WithCancel(context.Background())
	waiting := startDynamicKeyAcquire(h, ctx, 1)
	waitDynamicKeyState(t, h, 1, 1, 1)
	cancel()
	if result := receiveDynamicKey(t, waiting); result.ok || result.release != nil {
		t.Fatal("canceled waiter admitted")
	}
	waitDynamicKeyState(t, h, 1, 1, 0)
	release()
}

func TestKeyDynamicWaitTimeoutDoesNotLeakHolder(t *testing.T) {
	h := &Handler{AccountWait: 15 * time.Millisecond}
	release, _ := h.acquireKey(context.Background(), "key", "holder", 1)
	waiting := startDynamicKeyAcquire(h, context.Background(), 1)
	waitDynamicKeyState(t, h, 1, 1, 1)
	if result := receiveDynamicKey(t, waiting); result.ok || result.release != nil {
		t.Fatal("busy key admitted at wait deadline")
	}
	waitDynamicKeyState(t, h, 1, 1, 0)
	release()
	waitDynamicKeyState(t, h, 0, 1, 0)
}

type dynamicSharedKeyLimiter struct {
	calls   int
	ok      bool
	err     error
	release func()
}

func (s *dynamicSharedKeyLimiter) AcquireKeyWait(context.Context, string, int, string, time.Duration) (func(), bool, error) {
	s.calls++
	return s.release, s.ok, s.err
}

func TestKeyDynamicSharedBranchAndFallbackPolicy(t *testing.T) {
	t.Run("sharedSuccess", func(t *testing.T) {
		released := false
		shared := &dynamicSharedKeyLimiter{ok: true, release: func() { released = true }}
		h := &Handler{KeyLimiter: shared}
		release, ok := h.acquireKey(context.Background(), "key", "shared", 1)
		if !ok || shared.calls != 1 || len(h.keyGates) != 0 {
			t.Fatal("shared success unexpectedly used local gate")
		}
		release()
		if !released {
			t.Fatal("shared release was not returned")
		}
	})
	t.Run("sharedFullNoFallback", func(t *testing.T) {
		shared := &dynamicSharedKeyLimiter{}
		h := &Handler{KeyLimiter: shared}
		if _, ok := h.acquireKey(context.Background(), "key", "full", 1); ok || len(h.keyGates) != 0 {
			t.Fatal("shared full incorrectly fell back to local gate")
		}
	})
	t.Run("sharedFailureClosedByDefault", func(t *testing.T) {
		shared := &dynamicSharedKeyLimiter{err: errors.New("unavailable")}
		h := &Handler{KeyLimiter: shared}
		if release, ok, err := h.acquireKeyResult(context.Background(), "key", "closed", 1); ok || err == nil || release != nil || len(h.keyGates) != 0 {
			t.Fatalf("shared outage must not silently exceed global cap: admitted=%v err=%v", ok, err)
		}
	})
	t.Run("sharedFailureExplicitLocalFallback", func(t *testing.T) {
		shared := &dynamicSharedKeyLimiter{err: errors.New("unavailable")}
		h := &Handler{KeyLimiter: shared, SharedLimitFailOpen: true}
		release, ok := h.acquireKey(context.Background(), "key", "fallback", 1)
		if !ok || shared.calls != 1 {
			t.Fatal("shared error did not preserve local fallback policy")
		}
		waitDynamicKeyState(t, h, 1, 1, 0)
		release()
	})
	t.Run("disabledSkipsShared", func(t *testing.T) {
		shared := &dynamicSharedKeyLimiter{err: errors.New("must not be called")}
		h := &Handler{KeyLimiter: shared}
		release, ok := h.acquireKey(context.Background(), "key", "off", -1)
		if !ok || shared.calls != 0 {
			t.Fatal("disabled gate unexpectedly called shared limiter")
		}
		release()
	})
}
