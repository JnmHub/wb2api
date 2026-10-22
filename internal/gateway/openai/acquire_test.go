package openai

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wb2api/internal/pool"
)

func TestLocalFullAccountWaitReservesAndReleases(t *testing.T) {
	up := newRawUpstream(t, func(_ int, _ rawReq, c net.Conn) { sseOK(c) })
	f := newFixture(t, up, accSpec{})
	a := f.pool.Get("acct-0")
	a.SetMaxInflight(1)
	release, ok := a.TryAcquire(0)
	if !ok {
		t.Fatal("initial slot")
	}
	defer release()
	f.h.AccountWait = time.Second
	timer := time.AfterFunc(60*time.Millisecond, release)
	defer timer.Stop()
	code, body, _ := f.post(t, conv(1), nil)
	if code != 200 {
		t.Fatalf("local queue should serve after release: %d %s", code, body)
	}
	if a.Inflight() != 0 {
		t.Fatalf("leaked local slots: %d", a.Inflight())
	}
}

func TestPinnedLocalFullAccountWaitAndRateLimit(t *testing.T) {
	for _, wait := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_wait", true: "wait"}[wait], func(t *testing.T) {
			up := newRawUpstream(t, func(_ int, _ rawReq, c net.Conn) { sseOK(c) })
			f := newFixture(t, up, accSpec{})
			a := f.pool.Get("acct-0")
			a.SetMaxInflight(1)
			release, ok := a.TryAcquire(0)
			if !ok {
				t.Fatal("initial slot")
			}
			defer release()
			if wait {
				f.h.AccountWait = time.Second
				timer := time.AfterFunc(60*time.Millisecond, release)
				defer timer.Stop()
			}
			srv := httptest.NewServer(PinnedRoutes(f.pool, f.h))
			defer srv.Close()
			resp, err := http.Post(srv.URL+"/u/acct-0/v1/chat/completions", "application/json", strings.NewReader(conv(1)))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			want := http.StatusTooManyRequests
			if wait {
				want = http.StatusOK
			}
			if resp.StatusCode != want {
				t.Fatalf("want %d got %d: %s", want, resp.StatusCode, body)
			}
			if wait && a.Inflight() != 0 {
				t.Fatalf("leaked slots: %d", a.Inflight())
			}
		})
	}
}

func TestSharedWaitFailureReleasesLocalReservation(t *testing.T) {
	up := newRawUpstream(t, func(_ int, _ rawReq, c net.Conn) { sseOK(c) })
	f := newFixture(t, up, accSpec{})
	a := f.pool.Get("acct-0")
	lim := &waitLimiter{busy: map[string]bool{a.UID: true}}
	f.h.Limiter = lim
	f.h.AccountInflight = 1
	f.h.AccountWait = time.Second
	q := &request{r: httptest.NewRequest(http.MethodPost, "/", nil), model: "test"}
	q.at.full = map[string]*pool.Account{a.UID: a}
	q.at.tried = map[string]struct{}{}
	if _, _, ok := f.h.waitForFullAccount(q); ok {
		t.Fatal("full shared gate unexpectedly admitted request")
	}
	if lim.waits != 1 || a.Inflight() != 0 {
		t.Fatalf("waits=%d local slots=%d; want one bounded wait and no leak", lim.waits, a.Inflight())
	}
}

func TestLocalWaitCancellationDoesNotLeak(t *testing.T) {
	up := newRawUpstream(t, func(_ int, _ rawReq, c net.Conn) { sseOK(c) })
	f := newFixture(t, up, accSpec{})
	a := f.pool.Get("acct-0")
	a.SetMaxInflight(1)
	release, _ := a.TryAcquire(0)
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	q := &request{r: httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx), model: "test"}
	q.at.full = map[string]*pool.Account{a.UID: a}
	q.at.tried = map[string]struct{}{}
	f.h.AccountWait = time.Second
	if _, _, ok := f.h.waitForFullAccount(q); ok {
		t.Fatal("cancelled request acquired slot")
	}
	if a.Inflight() != 1 {
		t.Fatalf("changed existing lease: %d", a.Inflight())
	}
}
