package redisx

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestLimiterAccountSlots(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := Open(context.Background(), mr.Addr(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	l := NewLimiter(c)
	ctx := context.Background()
	rel1, ok, err := l.Acquire(ctx, "acct", 2, "r1")
	if err != nil || !ok {
		t.Fatalf("first slot: %v %v", ok, err)
	}
	defer rel1()
	rel2, ok, err := l.Acquire(ctx, "acct", 2, "r2")
	if err != nil || !ok {
		t.Fatalf("second slot: %v %v", ok, err)
	}
	defer rel2()
	rel3, ok, err := l.Acquire(ctx, "acct", 2, "r3")
	if rel3 != nil {
		rel3()
	}
	if err != nil || ok {
		t.Fatalf("third slot should be rejected: ok=%v err=%v", ok, err)
	}
	rel1()
	rel3, ok, err = l.Acquire(ctx, "acct", 2, "r3")
	if err != nil || !ok {
		t.Fatalf("slot after release: %v %v", ok, err)
	}
	defer rel3()
	rel2()
}

func TestLimiterRenewsLeaseUntilReleased(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := Open(context.Background(), mr.Addr(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	l := NewLimiter(c)
	// Leave scheduling headroom when all packages run concurrently on CI.
	l.ttl = time.Second
	rel, ok, err := l.Acquire(context.Background(), "acct", 1, "long-request")
	if err != nil || !ok {
		t.Fatalf("acquire lease: ok=%v err=%v", ok, err)
	}
	defer rel()
	time.Sleep(3 * l.ttl)
	competitor, ok, err := l.Acquire(context.Background(), "acct", 1, "competitor")
	if competitor != nil {
		competitor()
	}
	if err != nil || ok {
		t.Fatalf("active lease should remain full after TTL: ok=%v err=%v", ok, err)
	}
	rel()
	competitor, ok, err = l.Acquire(context.Background(), "acct", 1, "competitor")
	if err != nil || !ok {
		t.Fatalf("released lease should free slot: ok=%v err=%v", ok, err)
	}
	defer competitor()
}

func TestLimiterAcquireWaitQueuesForSlot(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := Open(context.Background(), mr.Addr(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	l := NewLimiter(c)
	ctx := context.Background()
	rel, ok, err := l.Acquire(ctx, "acct", 1, "held")
	if err != nil || !ok {
		t.Fatalf("seed slot: %v %v", ok, err)
	}
	defer rel()
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(80 * time.Millisecond)
		rel()
	}()
	defer func() { <-released }()
	start := time.Now()
	waitRel, ok, err := l.AcquireWait(ctx, "acct", 1, "queued", 2*time.Second)
	if err != nil || !ok {
		t.Fatalf("waiter did not get the released slot: %v %v", ok, err)
	}
	defer waitRel()
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("waiter returned too fast (%v), should have queued", elapsed)
	}
}

func TestLimiterAcquireWaitTimesOut(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := Open(context.Background(), mr.Addr(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	l := NewLimiter(c)
	ctx := context.Background()
	rel, ok, err := l.Acquire(ctx, "acct", 1, "held")
	if err != nil || !ok {
		t.Fatalf("seed slot: %v %v", ok, err)
	}
	defer rel()
	start := time.Now()
	waitRel, ok, err := l.AcquireWait(ctx, "acct", 1, "queued", 120*time.Millisecond)
	if waitRel != nil {
		waitRel()
	}
	if err != nil || ok {
		t.Fatalf("expected timeout, got ok=%v err=%v", ok, err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("timeout returned too early: %v", elapsed)
	}
}

func TestLimiterAcquireWaitCancels(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := Open(context.Background(), mr.Addr(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	l := NewLimiter(c)
	rel, ok, err := l.Acquire(context.Background(), "acct", 1, "held")
	if err != nil || !ok {
		t.Fatalf("seed slot: %v %v", ok, err)
	}
	defer rel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	waitRel, ok, err := l.AcquireWait(ctx, "acct", 1, "queued", 2*time.Second)
	if waitRel != nil {
		waitRel()
	}
	if err == nil {
		t.Fatalf("expected context cancellation, got ok=%v", ok)
	}
}

func TestLimiterKeySlotsSharedAcrossLimiters(t *testing.T) {
	mr := miniredis.RunT(t)
	c1, err := Open(context.Background(), mr.Addr(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	c2, err := Open(context.Background(), mr.Addr(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	l1 := NewLimiter(c1)
	l2 := NewLimiter(c2)
	ctx := context.Background()
	rel, ok, err := l1.AcquireKey(ctx, "shared-key", 1, "req-1")
	if err != nil || !ok {
		t.Fatalf("first slot: %v %v", ok, err)
	}
	defer rel()
	rel2, ok, err := l2.AcquireKey(ctx, "shared-key", 1, "req-2")
	if rel2 != nil {
		rel2()
	}
	if err != nil || ok {
		t.Fatalf("a second limiter must see the shared key slot as full: ok=%v err=%v", ok, err)
	}
	rel()
	rel2, ok, err = l2.AcquireKey(ctx, "shared-key", 1, "req-2")
	if err != nil || !ok {
		t.Fatalf("release on one limiter must free the key slot for another: ok=%v err=%v", ok, err)
	}
	defer rel2()
}

func TestLimiterKeySlotsAreSeparateFromAccountSlots(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := Open(context.Background(), mr.Addr(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	l := NewLimiter(c)
	ctx := context.Background()
	rel, ok, err := l.AcquireKey(ctx, "key-a", 1, "req-1")
	if err != nil || !ok {
		t.Fatalf("first key slot: %v %v", ok, err)
	}
	defer rel()
	rel2, ok, err := l.AcquireKey(ctx, "key-a", 1, "req-2")
	if rel2 != nil {
		rel2()
	}
	if err != nil || ok {
		t.Fatalf("second request on a full key should be rejected: ok=%v err=%v", ok, err)
	}
	rel3, ok, err := l.AcquireKey(ctx, "key-b", 1, "req-3")
	if err != nil || !ok {
		t.Fatalf("a different key must have its own slots: ok=%v err=%v", ok, err)
	}
	defer rel3()
	rel()
	rel2, ok, err = l.AcquireKey(ctx, "key-a", 1, "req-2")
	if err != nil || !ok {
		t.Fatalf("released key slot must be reusable: ok=%v err=%v", ok, err)
	}
	defer rel2()
}
