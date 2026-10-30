package redisx

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func newWindowStore(t *testing.T) (*ModelWindows, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := Open(context.Background(), mr.Addr(), "", 0)
	if err != nil {
		t.Fatalf("open redis: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return NewModelWindows(c), mr
}

func TestModelWindowsMarkAndRead(t *testing.T) {
	w, _ := newWindowStore(t)
	ctx := context.Background()
	until := time.Now().Add(10 * time.Minute)
	if err := w.Mark(ctx, "intl", "deepseek-v4.1-flash", "acct-1", until); err != nil {
		t.Fatal(err)
	}
	got, err := w.Blocked(ctx, "intl", "deepseek-v4.1-flash", []string{"acct-1", "acct-2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("blocked = %v, want only acct-1", got)
	}
	if d := time.Until(got["acct-1"]); d < 9*time.Minute {
		t.Fatalf("window ends too early: %v", d)
	}
	// A different model is a different window.
	other, err := w.Blocked(ctx, "intl", "space-bunny", []string{"acct-1"})
	if err != nil || len(other) != 0 {
		t.Fatalf("bunny blocked = %v (%v)", other, err)
	}
}

func TestModelWindowsExpireAndClear(t *testing.T) {
	w, mr := newWindowStore(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Second)
	// Mark refuses a window that already ended.
	if err := w.Mark(ctx, "intl", "m", "acct-1", past); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.Blocked(ctx, "intl", "m", []string{"acct-1"}); len(got) != 0 {
		t.Fatalf("expired window still blocks: %v", got)
	}

	if err := w.Mark(ctx, "intl", "m", "acct-1", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := w.Clear(ctx, "intl", "m", "acct-1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.Blocked(ctx, "intl", "m", []string{"acct-1"}); len(got) != 0 {
		t.Fatalf("cleared window still blocks: %v", got)
	}
	_ = mr
}

func TestModelWindowsSurviveReplicaRestart(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx := context.Background()
	until := time.Now().Add(20 * time.Minute)

	first, err := Open(ctx, mr.Addr(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewModelWindows(first).Mark(ctx, "cn", "glm-5.3", "acct-9", until); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()

	// A second replica (or a restarted process) sees the same window.
	second, err := Open(ctx, mr.Addr(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	got, err := NewModelWindows(second).Blocked(ctx, "cn", "glm-5.3", []string{"acct-9"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["acct-9"]; !ok {
		t.Fatalf("window not shared across clients: %v", got)
	}
}

func TestModelWindowsNilClient(t *testing.T) {
	w := NewModelWindows(nil)
	if err := w.Mark(context.Background(), "intl", "m", "a", time.Now().Add(time.Minute)); err == nil {
		t.Fatal("expected an error without a client")
	}
	if _, err := w.Blocked(context.Background(), "intl", "m", []string{"a"}); err == nil {
		t.Fatal("expected an error without a client")
	}
}
