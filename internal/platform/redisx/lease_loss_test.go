package redisx

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestLeaseLossCancelsOwnerWhenReservationRemoved(t *testing.T) {
	for _, key := range []bool{false, true} {
		t.Run(map[bool]string{false: "account", true: "key"}[key], func(t *testing.T) {
			mr := miniredis.RunT(t)
			c, err := Open(context.Background(), mr.Addr(), "", 0)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			l := NewLimiter(c)
			l.ttl = 300 * time.Millisecond
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			ctx = l.WithLeaseLoss(ctx, cancel)
			var rel func()
			var ok bool
			name := "wb:inflight:a"
			if key {
				name = "wb:keyinflight:a"
				rel, ok, err = l.AcquireKey(ctx, "a", 1, "owner")
			} else {
				rel, ok, err = l.Acquire(ctx, "a", 1, "owner")
			}
			if err != nil || !ok {
				t.Fatalf("acquire=%v %v", ok, err)
			}
			defer rel()
			mr.Del(name)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
				t.Fatal("lost lease did not cancel owner")
			}
			if !errors.Is(context.Cause(ctx), ErrLeaseLost) {
				t.Fatalf("cause=%v", context.Cause(ctx))
			}
			if mr.Exists(name) {
				t.Fatal("renew resurrected removed lease")
			}
		})
	}
}

func TestLeaseScriptsUseServerClockInsteadOfReplicaClock(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := Open(context.Background(), mr.Addr(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	base := time.Unix(1700000000, 0)
	mr.SetTime(base)
	key := "test:clock:slot"
	const ttl = int64(10000)
	// A slow replica must store the Redis time, not its own wall clock.
	if n, err := acquireScript.Run(ctx, c.Client, []string{key}, base.Add(-time.Hour).UnixMilli(), ttl, 1, "held").Int(); err != nil || n != 1 {
		t.Fatalf("acquire n=%d err=%v", n, err)
	}
	if score := c.ZScore(ctx, key, "held").Val(); score != float64(base.UnixMilli()) {
		t.Fatalf("lease stored replica clock: %v", score)
	}
	// A fast replica must not prune the still-live holder and over-admit.
	if n, err := acquireScript.Run(ctx, c.Client, []string{key}, base.Add(time.Hour).UnixMilli(), ttl, 1, "other").Int(); err != nil || n != 0 {
		t.Fatalf("fast replica removed healthy holder: n=%d err=%v", n, err)
	}
	mr.SetTime(base.Add(time.Second))
	if n, err := renewScript.Run(ctx, c.Client, []string{key}, "held", base.Add(time.Hour).UnixMilli(), ttl).Int(); err != nil || n != 1 {
		t.Fatalf("fast renewal rejected live owner: n=%d err=%v", n, err)
	}
	if score := c.ZScore(ctx, key, "held").Val(); score != float64(base.Add(time.Second).UnixMilli()) {
		t.Fatalf("renewal stored replica clock: %v", score)
	}
}

func TestLeaseWatchDoesNotExtendOnUnconfirmedRenewal(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	ctx = NewLimiter(nil).WithLeaseLoss(ctx, cancel)
	w := newLeaseWatch(ctx, time.Now(), 80*time.Millisecond)
	defer w.stop()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("unconfirmed lease did not expire")
	}
	if !errors.Is(context.Cause(ctx), ErrLeaseLost) {
		t.Fatalf("cause=%v", context.Cause(ctx))
	}
}

func TestLeaseWatchLateAcknowledgmentCannotReviveExpiredWindow(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	ctx = NewLimiter(nil).WithLeaseLoss(ctx, cancel)
	w := newLeaseWatch(ctx, time.Now(), time.Hour)
	defer w.stop()
	// Model a scheduler-delayed timer callback, not a real one-hour sleep.
	w.mu.Lock()
	w.deadline = time.Now().Add(-time.Millisecond)
	w.mu.Unlock()
	w.confirm(time.Now(), time.Hour)
	if !errors.Is(context.Cause(ctx), ErrLeaseLost) {
		t.Fatalf("late acknowledgment revived unsafe owner: %v", context.Cause(ctx))
	}
}

func TestLeaseWatchStopJoinsPendingLossCallback(t *testing.T) {
	entered, finish := make(chan struct{}), make(chan struct{})
	ctx := NewLimiter(nil).WithLeaseLoss(context.Background(), func(error) {
		close(entered)
		<-finish
	})
	w := newLeaseWatch(ctx, time.Now(), time.Hour)
	w.mu.Lock()
	w.deadline = time.Now().Add(-time.Millisecond)
	w.mu.Unlock()
	go w.expire()
	<-entered
	stopped := make(chan struct{})
	go func() { w.stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("stop returned before pending cancellation callback finished")
	case <-time.After(10 * time.Millisecond):
	}
	close(finish)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("stop did not join completed callback")
	}
}

func TestLeaseWatchServerRejectionAndTimerNotifyOnce(t *testing.T) {
	var calls atomic.Int32
	ctx := NewLimiter(nil).WithLeaseLoss(context.Background(), func(error) { calls.Add(1) })
	w := newLeaseWatch(ctx, time.Now(), time.Hour)
	w.mu.Lock()
	w.deadline = time.Now().Add(-time.Millisecond)
	w.mu.Unlock()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); w.expire() }()
	go func() { defer wg.Done(); w.lose() }()
	wg.Wait()
	w.stop()
	if got := calls.Load(); got != 1 {
		t.Fatalf("loss callback called %d times, want 1", got)
	}
}

func TestLeaseWatchStaleCallbackAndStop(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	ctx = NewLimiter(nil).WithLeaseLoss(ctx, cancel)
	w := newLeaseWatch(ctx, time.Now(), time.Hour)
	w.expire()
	if ctx.Err() != nil {
		t.Fatal("stale callback canceled new lease")
	}
	w.stop()
	w.expire()
	if ctx.Err() != nil {
		t.Fatal("released lease canceled request")
	}
}
