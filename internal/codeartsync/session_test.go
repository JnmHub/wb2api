package codeartsync

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
)

type heartbeatSpy struct {
	mu       sync.Mutex
	busy     []string
	idle     []string
	failBusy bool
	srv      *httptest.Server
}

func newHeartbeatSpy(t *testing.T) *heartbeatSpy {
	t.Helper()
	spy := &heartbeatSpy{}
	spy.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/snap-manager/v1/chat-session/heartbeat" {
			t.Errorf("path = %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		id := r.Header.Get("user-session-id")
		spy.mu.Lock()
		switch r.URL.Query().Get("status") {
		case "busy":
			if spy.failBusy {
				spy.mu.Unlock()
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error_code":"TM.00001041","error_msg":"session limit"}`)
				return
			}
			spy.busy = append(spy.busy, id)
		case "idle":
			spy.idle = append(spy.idle, id)
		}
		spy.mu.Unlock()
		fmt.Fprint(w, `{"status":"ok"}`)
	}))
	t.Cleanup(spy.srv.Close)
	return spy
}

func (s *heartbeatSpy) counts() (busy, idle int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.busy), len(s.idle)
}

func sessionPool(uid string) *pool.Pool {
	return pool.New([]*accounts.Record{{
		UID: uid, Realm: realm.CodeArts, AccessToken: "STS",
		Extra: map[string]any{"codearts": map[string]any{"access_key_id": "AK", "secret_access_key": "SK"}},
	}})
}

func TestSessionsRegisterBusyAndReleaseIdle(t *testing.T) {
	spy := newHeartbeatSpy(t)
	p := sessionPool("hw-1")
	sm := &Sessions{Pool: p, Base: spy.srv.URL, Interval: time.Hour}
	sess, err := sm.Begin(context.Background(), p.Get("hw-1"))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if sess.ID() == "" {
		t.Fatal("no session id")
	}
	if busy, idle := spy.counts(); busy != 1 || idle != 0 {
		t.Fatalf("busy=%d idle=%d", busy, idle)
	}
	if sm.Active("hw-1") != 1 {
		t.Fatalf("active = %d", sm.Active("hw-1"))
	}
	sess.Close()
	if busy, idle := spy.counts(); busy != 1 || idle != 1 {
		t.Fatalf("after close busy=%d idle=%d", busy, idle)
	}
	if sm.Active("hw-1") != 0 {
		t.Fatalf("active after close = %d", sm.Active("hw-1"))
	}
	// Close is idempotent: idle must be sent exactly once.
	sess.Close()
	if _, idle := spy.counts(); idle != 1 {
		t.Fatalf("idle sent %d times", idle)
	}
}

// The upstream allows three conversations; the fourth locally becomes
// "pick another account" instead of a TM.00001041 from the upstream.
func TestSessionsLimitBeforeHittingUpstream(t *testing.T) {
	spy := newHeartbeatSpy(t)
	p := sessionPool("hw-1")
	limit := realm.CodeArtsInflight()
	sm := &Sessions{Pool: p, Base: spy.srv.URL, Interval: time.Hour,
		Limit: func(*accounts.Record) int { return limit }}
	held := make([]*Session, 0, limit)
	for i := 0; i < limit; i++ {
		sess, err := sm.Begin(context.Background(), p.Get("hw-1"))
		if err != nil {
			t.Fatalf("Begin %d: %v", i, err)
		}
		held = append(held, sess)
	}
	if _, err := sm.Begin(context.Background(), p.Get("hw-1")); !errors.Is(err, ErrSessionsFull) {
		t.Fatalf("err = %v, want ErrSessionsFull", err)
	}
	if busy, _ := spy.counts(); busy != limit {
		t.Fatalf("busy = %d, want %d (the rejected attempt must not touch the upstream)", busy, limit)
	}
	for _, s := range held {
		s.Close()
	}
	if _, err := sm.Begin(context.Background(), p.Get("hw-1")); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

// A rejected busy must release the id it just invented.
func TestSessionsReleaseAfterBusyFailure(t *testing.T) {
	spy := newHeartbeatSpy(t)
	spy.failBusy = true
	p := sessionPool("hw-1")
	sm := &Sessions{Pool: p, Base: spy.srv.URL, Interval: time.Hour}
	if _, err := sm.Begin(context.Background(), p.Get("hw-1")); err == nil {
		t.Fatal("expected the busy rejection to surface")
	}
	if sm.Active("hw-1") != 0 {
		t.Fatalf("slot leaked: %d", sm.Active("hw-1"))
	}
	if _, idle := spy.counts(); idle != 1 {
		t.Fatalf("idle after failure = %d, want 1", idle)
	}
}

// The lease is renewed while it is held, and renewal stops after Close.
func TestSessionsRenewWhileHeld(t *testing.T) {
	spy := newHeartbeatSpy(t)
	p := sessionPool("hw-1")
	sm := &Sessions{Pool: p, Base: spy.srv.URL, Interval: 15 * time.Millisecond}
	sess, err := sm.Begin(context.Background(), p.Get("hw-1"))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if busy, _ := spy.counts(); busy >= 3 {
			break
		}
		if time.Now().After(deadline) {
			busy, _ := spy.counts()
			t.Fatalf("busy renewals = %d, want >= 3", busy)
		}
		time.Sleep(10 * time.Millisecond)
	}
	sess.Close()
	busyAfter, _ := spy.counts()
	time.Sleep(60 * time.Millisecond)
	busyLater, idle := spy.counts()
	if busyLater != busyAfter {
		t.Fatalf("renewals continued after close: %d -> %d", busyAfter, busyLater)
	}
	if idle != 1 {
		t.Fatalf("idle = %d", idle)
	}
}

var _ = strings.TrimSpace
