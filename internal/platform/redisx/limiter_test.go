package redisx

import (
	"context"
	"testing"

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
