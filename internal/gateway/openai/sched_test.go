package openai

import (
	"context"
	"net"
	"testing"
	"time"
)

type fakeEscape struct{ uid string }

func (f fakeEscape) ShouldEscape(uid string) bool { return uid == f.uid }
func (f fakeEscape) Score(string) (float64, float64, bool) {
	return 0, 0, false
}

type waitLimiter struct {
	busy  map[string]bool
	waits int
}

func (l *waitLimiter) Acquire(_ context.Context, uid string, _ int, _ string) (func(), bool, error) {
	if l.busy[uid] {
		return nil, false, nil
	}
	return func() {}, true, nil
}

func (l *waitLimiter) AcquireWait(_ context.Context, uid string, _ int, _ string, _ time.Duration) (func(), bool, error) {
	l.waits++
	return nil, false, nil
}

type busyLimiter struct{ busy map[string]bool }

func (b busyLimiter) Acquire(_ context.Context, uid string, _ int, _ string) (func(), bool, error) {
	if b.busy[uid] {
		return nil, false, nil
	}
	return func() {}, true, nil
}

func TestStickySessionUsesSoleSlowAccountWhenNoAlternativeExists(t *testing.T) {
	up := newRawUpstream(t, func(n int, r rawReq, c net.Conn) { sseOK(c) })
	f := newFixture(t, up, accSpec{})
	f.h.Sched = fakeEscape{uid: "acct-0"}
	f.pool.Affinity().Bind("sess", "acct-0")

	code, body, _ := f.post(t, conv(1), map[string]string{"X-Session-Id": "sess"})
	if code != 200 {
		t.Fatalf("a slow but otherwise healthy sole account must remain usable: %d %s", code, body)
	}
	if got := f.pool.Affinity().Get("sess"); got != "acct-0" {
		t.Fatalf("sole usable account affinity changed to %q", got)
	}
}

func TestStickySessionEscapesSlowAccount(t *testing.T) {
	up := newRawUpstream(t, func(n int, r rawReq, c net.Conn) { sseOK(c) })
	f := newFixture(t, up, accSpec{}, accSpec{})
	f.h.Sched = fakeEscape{uid: "acct-0"}
	f.pool.Affinity().Bind("sess", "acct-0")

	code, body, _ := f.post(t, conv(1), map[string]string{"X-Session-Id": "sess"})
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	if got := f.pool.Affinity().Get("sess"); got != "acct-1" {
		t.Fatalf("sticky session should move to the healthy account, got %q", got)
	}
}

func TestBusyAccountDoesNotConsumeFailoverAttempts(t *testing.T) {
	up := newRawUpstream(t, func(n int, r rawReq, c net.Conn) { sseOK(c) })
	f := newFixture(t, up, accSpec{}, accSpec{})
	f.h.Limiter = &waitLimiter{busy: map[string]bool{"acct-0": true}}
	f.h.AccountInflight = 1
	f.pool.Affinity().Bind("sess", "acct-0")

	code, body, _ := f.post(t, conv(1), map[string]string{"X-Session-Id": "sess"})
	if code != 200 {
		t.Fatalf("a busy warm account must not exhaust the request: %d %s", code, body)
	}
	if got := f.pool.Affinity().Get("sess"); got != "acct-1" {
		t.Fatalf("request should move to the free account, got %q", got)
	}
}

func TestFullStickyAccountDoesNotWaitWhenAnotherIsFree(t *testing.T) {
	up := newRawUpstream(t, func(n int, r rawReq, c net.Conn) { sseOK(c) })
	f := newFixture(t, up, accSpec{}, accSpec{})
	lim := &waitLimiter{busy: map[string]bool{"acct-0": true}}
	f.h.Limiter = lim
	f.h.AccountInflight = 1
	f.h.AccountWait = 5 * time.Second
	f.pool.Affinity().Bind("sess", "acct-0")

	code, body, _ := f.post(t, conv(1), map[string]string{"X-Session-Id": "sess"})
	if code != 200 {
		t.Fatalf("free account should serve immediately: %d %s", code, body)
	}
	if lim.waits != 0 {
		t.Fatalf("must probe free accounts before waiting, got %d waits", lim.waits)
	}
	if got := f.pool.Affinity().Get("sess"); got != "acct-1" {
		t.Fatalf("request should move to the free account, got %q", got)
	}
}

func TestAllAccountsFullReturnsRetryableRateLimit(t *testing.T) {
	up := newRawUpstream(t, func(n int, r rawReq, c net.Conn) { sseOK(c) })
	f := newFixture(t, up, accSpec{}, accSpec{})
	lim := &waitLimiter{busy: map[string]bool{"acct-0": true, "acct-1": true}}
	f.h.Limiter = lim
	f.h.AccountInflight = 1
	f.h.AccountWait = 25 * time.Millisecond

	code, body, _ := f.post(t, conv(1), nil)
	if code != 429 {
		t.Fatalf("all-full pool should return 429, got %d: %s", code, body)
	}
	if lim.waits == 0 {
		t.Fatal("all-full pool should enter the bounded wait phase")
	}
}

func TestStickyPicksLeastLoadedWarmAccount(t *testing.T) {
	up := newRawUpstream(t, func(n int, r rawReq, c net.Conn) { sseOK(c) })
	f := newFixture(t, up, accSpec{}, accSpec{})
	f.pool.Affinity().Bind("sess", "acct-1")
	f.pool.Affinity().Bind("sess", "acct-0") // acct-0 is the most recent
	busy := f.pool.Get("acct-0")
	busy.Acquire()
	defer busy.Release()

	code, body, _ := f.post(t, conv(1), map[string]string{"X-Session-Id": "sess"})
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	if got := f.pool.Affinity().Get("sess"); got != "acct-1" {
		t.Fatalf("least-loaded warm account should win, got %q", got)
	}
}
