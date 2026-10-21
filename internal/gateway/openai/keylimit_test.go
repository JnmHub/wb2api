package openai

import (
	"context"
	"testing"
	"time"
)

func TestAPIKeyConcurrencyGate(t *testing.T) {
	h := &Handler{AccountWait: 60 * time.Millisecond}
	release, ok := h.acquireKey(context.Background(), "k1", "req-1", 1)
	if !ok {
		t.Fatal("first slot must be granted")
	}
	start := time.Now()
	if _, ok := h.acquireKey(context.Background(), "k1", "req-2", 1); ok {
		t.Fatal("second slot must not be granted while the first is held")
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Fatalf("busy key returned too early: %v", elapsed)
	}
	release()
	release2, ok := h.acquireKey(context.Background(), "k1", "req-3", 1)
	if !ok {
		t.Fatal("slot must be reusable after release")
	}
	release2()
}

func TestAPIKeyConcurrencyGateSeparateKeys(t *testing.T) {
	h := &Handler{}
	r1, ok := h.acquireKey(context.Background(), "a", "req-a", 1)
	if !ok {
		t.Fatal("key a")
	}
	r2, ok := h.acquireKey(context.Background(), "b", "req-b", 1)
	if !ok {
		t.Fatal("key b must not share key a's gate")
	}
	r1()
	r2()
}
