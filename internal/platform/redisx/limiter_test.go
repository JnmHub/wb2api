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
	rel2, ok, err := l.Acquire(ctx, "acct", 2, "r2")
	if err != nil || !ok {
		t.Fatalf("second slot: %v %v", ok, err)
	}
	if _, ok, _ := l.Acquire(ctx, "acct", 2, "r3"); ok {
		t.Fatal("third slot should be rejected")
	}
	rel1()
	if _, ok, err := l.Acquire(ctx, "acct", 2, "r3"); err != nil || !ok {
		t.Fatalf("slot after release: %v %v", ok, err)
	}
	rel2()
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
	go func() {
		time.Sleep(80 * time.Millisecond)
		rel()
	}()
	start := time.Now()
	waitRel, ok, err := l.AcquireWait(ctx, "acct", 1, "queued", 2*time.Second)
	if err != nil || !ok {
		t.Fatalf("waiter did not get the released slot: %v %v", ok, err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("waiter returned too fast (%v), should have queued", elapsed)
	}
	waitRel()
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
	if _, ok, err := l.AcquireWait(ctx, "acct", 1, "queued", 120*time.Millisecond); err != nil || ok {
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
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if _, ok, err := l.AcquireWait(ctx, "acct", 1, "queued", 2*time.Second); err == nil {
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
	if _, ok, _ := l2.AcquireKey(ctx, "shared-key", 1, "req-2"); ok {
		t.Fatal("a second limiter must see the shared key slot as full")
	}
	rel()
	if _, ok, _ := l2.AcquireKey(ctx, "shared-key", 1, "req-2"); !ok {
		t.Fatal("release on one limiter must free the key slot for another")
	}
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
	if _, ok, _ := l.AcquireKey(ctx, "key-a", 1, "req-2"); ok {
		t.Fatal("second request on a full key should be rejected")
	}
	if _, ok, _ := l.AcquireKey(ctx, "key-b", 1, "req-3"); !ok {
		t.Fatal("a different key must have its own slots")
	}
	rel()
	if _, ok, _ := l.AcquireKey(ctx, "key-a", 1, "req-2"); !ok {
		t.Fatal("released key slot must be reusable")
	}
}
