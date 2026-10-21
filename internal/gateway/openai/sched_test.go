package openai

import (
	"context"
	"net"
	"testing"
)

type fakeEscape struct{ uid string }

func (f fakeEscape) ShouldEscape(uid string) bool { return uid == f.uid }

type busyLimiter struct{ busy map[string]bool }

func (b busyLimiter) Acquire(_ context.Context, uid string, _ int, _ string) (func(), bool, error) {
	if b.busy[uid] {
		return nil, false, nil
	}
	return func() {}, true, nil
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
	f.h.Limiter = busyLimiter{busy: map[string]bool{"acct-0": true}}
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
