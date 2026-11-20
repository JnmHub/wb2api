package refresh

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream/raccoon"
)

func TestRaccoonRefreshBackoffRespectsRetryAfterAndRejection(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != raccoon.RefreshPath {
			t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
		}
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			// HTTP 429 always takes precedence over an auth-looking envelope.
			fmt.Fprint(w, "{\"code\":200003}")
			return
		}
		fmt.Fprint(w, "{\"code\":0,\"data\":{\"access_token\":\"new-at\",\"refresh_token\":\"new-rt\"}}")
	}))
	defer server.Close()

	now := time.Now().Truncate(time.Second)
	p := pool.New([]*accounts.Record{{UID: "rc-backoff", Realm: "raccoon", Enabled: true,
		AccessToken: "old-at", RefreshToken: "old-rt", ExpiresAt: now.Add(time.Minute).Unix()}})
	acc := p.Get("rc-backoff")
	r := &Refresher{Pool: p, Now: func() time.Time { return now }, BaseURL: func(*pool.Account) string { return server.URL }}
	initial := acc.Record()
	err := r.Refresh(t.Context(), acc)
	retryAt, ok := RetryTime(err)
	if !ok || !retryAt.Equal(now.Add(2*time.Minute)) || acc.Record().RaccoonNeedsLogin() || !acc.Ready("", now) {
		t.Fatalf("429 lost backoff or blocked valid AT: err=%v retryAt=%v state=%+v", err, retryAt, acc.Record().RaccoonAuthState())
	}
	if err := r.RefreshObserved(t.Context(), acc, initial); err == nil || !acc.Record().RaccoonAuthState().AccessRejected || acc.Ready("", now) {
		t.Fatalf("unexpected 401 should park only the observed AT: err=%v state=%+v", err, acc.Record().RaccoonAuthState())
	}
	now = now.Add(90 * time.Second)
	if r.due(acc, now) || calls.Load() != 1 {
		t.Fatalf("expired access token bypassed Retry-After, calls=%d", calls.Load())
	}
	now = now.Add(time.Minute)
	if n := r.ScanOnce(t.Context()); n != 1 || calls.Load() != 2 || acc.Record().AccessToken != "new-at" ||
		acc.Record().RefreshToken != "new-rt" || !acc.Ready("", now) {
		t.Fatalf("retry did not recover: scans=%d calls=%d state=%+v", n, calls.Load(), acc.Record().RaccoonAuthState())
	}
}

func TestRaccoonRefreshFailureToPersistRetainsRotatedToken(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, "{\"code\":0,\"data\":{\"access_token\":\"new-at\",\"refresh_token\":\"new-rt\"}}")
	}))
	defer server.Close()
	p := pool.New([]*accounts.Record{{UID: "rc-pending", Realm: "raccoon", Enabled: true, AccessToken: "old-at", RefreshToken: "old-rt"}})
	acc := p.Get("rc-pending")
	var writes atomic.Int32
	r := &Refresher{Pool: p, BaseURL: func(*pool.Account) string { return server.URL },
		Persist: func(rec *accounts.Record) error {
			if rec.RefreshToken == "new-rt" && writes.Add(1) == 1 {
				return errors.New("temporary store error")
			}
			return nil
		}}
	if err := r.Refresh(t.Context(), acc); err == nil || acc.Record().RefreshToken != "old-rt" {
		t.Fatalf("lost pending write: err=%v", err)
	}
	if err := r.Refresh(t.Context(), acc); err != nil || acc.Record().RefreshToken != "new-rt" || calls.Load() != 1 {
		t.Fatalf("retried upstream exchange rather than pending write: err=%v calls=%d", err, calls.Load())
	}
}

func TestRaccoonIAMWorksUntilExpiryAndNeverExchanges(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	expiry := now.Add(time.Minute)
	claims := fmt.Sprintf("{\"iss\":\"example.test/iam\",\"sub\":\"123\",\"exp\":%d}", expiry.Unix())
	token := "." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + "."
	p := pool.New([]*accounts.Record{{UID: "rc-iam", Realm: "raccoon", Enabled: true,
		AccessToken: token, ExpiresAt: expiry.Unix()}})
	acc := p.Get("rc-iam")
	r := &Refresher{Pool: p, Now: func() time.Time { return now },
		BaseURL: func(*pool.Account) string { panic("IAM has no refresh exchange") }}
	if r.due(acc, now) || !acc.Ready("", now) || acc.Record().RaccoonNeedsLogin() {
		t.Fatal("IAM was expired early")
	}
	if err := r.Refresh(t.Context(), acc); !errors.Is(err, raccoon.ErrNoRefreshToken) || acc.Record().RaccoonNeedsLogin() {
		t.Fatalf("manual IAM refresh should decline without a terminal state: %v", err)
	}
	now = expiry
	if !r.due(acc, now) || acc.Ready("", now) {
		t.Fatal("expired IAM was still eligible")
	}
	if n := r.ScanOnce(t.Context()); n != 1 || !acc.Record().RaccoonNeedsLogin() {
		t.Fatalf("IAM expiry not recorded, scans=%d", n)
	}
}

func TestRaccoonDisabledRefreshNeedsManualSelection(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, "{\"code\":0,\"data\":{\"access_token\":\"new-at\",\"refresh_token\":\"new-rt\"}}")
	}))
	defer server.Close()
	p := pool.New([]*accounts.Record{{UID: "rc-disabled", Realm: "raccoon", Enabled: false, AccessToken: "old-at", RefreshToken: "old-rt"}})
	acc := p.Get("rc-disabled")
	r := &Refresher{Pool: p, BaseURL: func(*pool.Account) string { return server.URL }}
	if err := r.Refresh(context.Background(), acc); !errors.Is(err, ErrRaccoonDisabled) || calls.Load() != 0 {
		t.Fatalf("background refresh of disabled account: err=%v calls=%d", err, calls.Load())
	}
	if err := r.Refresh(WithManualRaccoon(t.Context(), acc.UID), acc); err != nil || acc.Enabled() ||
		acc.Record().RefreshToken != "new-rt" || calls.Load() != 1 {
		t.Fatalf("manual refresh didn't preserve disabled state: err=%v enabled=%v calls=%d", err, acc.Enabled(), calls.Load())
	}
}

func TestRaccoonRefreshPreservesConcurrentAccountUpdates(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprintf("replace=%v", replace), func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				close(started)
				<-release
				fmt.Fprint(w, "{\"code\":0,\"data\":{\"access_token\":\"refreshed-at\",\"refresh_token\":\"refreshed-rt\"}}")
			}))
			defer server.Close()
			p := pool.New([]*accounts.Record{{UID: "rc-update", Realm: "raccoon", Enabled: true, AccessToken: "old-at", RefreshToken: "old-rt"}})
			acc := p.Get("rc-update")
			r := &Refresher{Pool: p, BaseURL: func(*pool.Account) string { return server.URL }}
			done := make(chan error, 1)
			go func() { done <- r.Refresh(context.Background(), acc) }()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				close(release)
				t.Fatal("refresh request did not start")
			}
			p.Update(acc.UID, func(next *accounts.Record) {
				next.Enabled = false
				next.Nickname = "new name"
				if replace {
					next.AccessToken, next.RefreshToken = "replacement-at", "replacement-rt"
				}
			})
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			wantRT := "refreshed-rt"
			if replace {
				wantRT = "replacement-rt"
			}
			if got := acc.Record(); got.RefreshToken != wantRT || got.IsEnabled() || got.Nickname != "new name" {
				t.Fatalf("refresh clobbered an operator update: enabled=%v nickname=%q expected RT installed=%v", got.IsEnabled(), got.Nickname, got.RefreshToken == wantRT)
			}
		})
	}
}
