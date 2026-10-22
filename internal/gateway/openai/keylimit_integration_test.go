package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/store/settings"
)

func TestKeyLimitIntegrationUnlimitedHolderSurvivesEnable(t *testing.T) {
	entered := make(chan int32, 4)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	// Register this before the servers so a failing test cannot leave Close
	// waiting on the upstream handler. Also unblock explicitly before returns.
	defer unblock()
	var calls atomic.Int32
	e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		entered <- n
		if n == 1 {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		sseUpstream("ok")(w, r)
	}), 2, "")
	t.Cleanup(unblock)
	e.h.AccountWait = 50 * time.Millisecond
	if e.h.KeyLimiter != nil {
		t.Fatal("regression requires the local key gate")
	}
	name, zero := "changing-key", 0
	key, err := e.h.Settings.CreateKey(settings.KeyInput{Name: &name, ConcurrencyLimit: &zero})
	if err != nil || key.ID == "" || key.ConcurrencyLimit != 0 {
		t.Fatalf("create unlimited key: key=%+v err=%v", key, err)
	}
	// Completion is sent only after ServeHTTP returned, including its deferred
	// key release. Reading client EOF alone need not prove deferred cleanup ran.
	completed := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.h.ServeHTTP(w, r)
		completed <- r.Header.Get("X-Test-Request")
	}))
	defer func() { unblock(); srv.Close() }()
	type reply struct {
		status     int
		body       string
		retryAfter string
		err        error
	}
	post := func(label string) reply {
		req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			return reply{err: err}
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key.Key)
		req.Header.Set("X-Test-Request", label)
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
		if err != nil {
			return reply{err: err}
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return reply{status: resp.StatusCode, body: string(body), retryAfter: resp.Header.Get("Retry-After"), err: err}
	}
	first := make(chan reply, 1)
	go func() { first <- post("unlimited") }()
	select {
	case n := <-entered:
		if n != 1 {
			t.Fatalf("first upstream entry=%d", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unlimited request never entered upstream")
	}
	// This observes actual upstream admission, not a sleep-based assumption
	// about when the goroutine reached chat_loop's key guard.
	e.h.keyMu.Lock()
	gate := e.h.keyGates[key.ID]
	counted := gate != nil && gate.active == 1 && gate.limit == 0
	e.h.keyMu.Unlock()
	if !counted {
		unblock()
		t.Fatal("chat_loop did not count the in-flight unlimited-key request")
	}
	one := 1
	if err := e.h.Settings.UpdateKey(key.ID, settings.KeyInput{ConcurrencyLimit: &one}); err != nil {
		unblock()
		t.Fatal(err)
	}
	keys := e.h.Settings.APIKeys(false)
	if len(keys) != 1 || keys[0].ID != key.ID || keys[0].ConcurrencyLimit != 1 {
		unblock()
		t.Fatalf("cap update did not preserve same key ID: %+v", keys)
	}
	blocked := post("capped")
	if blocked.err != nil || blocked.status != http.StatusTooManyRequests || blocked.retryAfter == "" || !strings.Contains(blocked.body, "rate_limit_error") {
		unblock()
		t.Fatalf("enabled cap must include old unlimited holder: %+v", blocked)
	}
	if calls.Load() != 1 {
		unblock()
		t.Fatalf("new request reached upstream while old holder active: calls=%d", calls.Load())
	}
	select {
	case n := <-entered:
		unblock()
		t.Fatalf("unexpected upstream entry before release: %d", n)
	default:
	}
	unblock()
	select {
	case result := <-first:
		if result.err != nil || result.status != http.StatusOK {
			t.Fatalf("original unlimited request failed: %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unlimited request did not finish after release")
	}
	finishedFirst := false
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for !finishedFirst {
		select {
		case label := <-completed:
			finishedFirst = label == "unlimited"
		case <-deadline.C:
			t.Fatal("original handler did not finish deferred key release")
		}
	}
	after := post("after-release")
	if after.err != nil || after.status != http.StatusOK || !strings.Contains(after.body, "ok") {
		t.Fatalf("same key not admitted after old holder released: %+v", after)
	}
	select {
	case n := <-entered:
		if n != 2 {
			t.Fatalf("post-release upstream entry=%d", n)
		}
	default:
		t.Fatal("successful post-release request did not enter upstream")
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("expected only original and post-release POSTs, got %d", n)
	}
}
