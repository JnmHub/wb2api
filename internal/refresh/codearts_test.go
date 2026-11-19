package refresh

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream"
	"wb2api/internal/upstream/codearts"
)

func codeartsRecord(t *testing.T, jwk codearts.DPoPKey) *accounts.Record {
	t.Helper()
	extra := map[string]any{
		"access_key_id": "AK-OLD", "secret_access_key": "SK-OLD",
		"security_token": "STS-OLD", "code_verifier": "verifier-1",
	}
	if jwk != nil {
		m := map[string]any{}
		for k, v := range jwk {
			m[k] = v
		}
		extra["dpop_private_key"] = m
	}
	return &accounts.Record{
		UID: "hw-1", Realm: realm.CodeArts, AccessToken: "STS-OLD",
		RefreshToken: "refresh-1", ExpiresAt: time.Now().Add(time.Minute).Unix(),
		Extra: map[string]any{"codearts": extra},
	}
}

// The refresher must not run the WorkBuddy refresh endpoint for a CodeArts
// account: the chain is OAuth2 + DPoP against snap-manager, and the AK/SK pair
// has to move with the new STS token.
func TestCodeArtsRefreshDispatch(t *testing.T) {
	var gotPath, gotForm, gotDPoP string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotDPoP = r.Header.Get("DPoP")
		_ = r.ParseForm()
		gotForm = r.PostForm.Encode()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"user_id":"hw-1","refresh_token":"refresh-2","credentials":{
			"access_key_id":"AK-NEW","secret_access_key":"SK-NEW","security_token":"STS-NEW",
			"expiration":"2026-12-01T00:00:00Z"}}`)
	}))
	defer srv.Close()

	jwk, err := codearts.NewDPoPKey()
	if err != nil {
		t.Fatal(err)
	}
	acc := pool.NewAccount(codeartsRecord(t, jwk))
	p := pool.New(nil)
	p.Add(acc.Record())

	var saved *accounts.Record
	r := &Refresher{
		Pool:    p,
		Up:      upstream.NewManager(upstream.DefaultTimeouts()),
		Persist: func(rec *accounts.Record) error { saved = rec; return nil },
		BaseURL: func(*pool.Account) string { return srv.URL },
	}
	live := p.Get("hw-1")
	if live == nil {
		t.Fatal("account missing from the pool")
	}
	if err := r.Refresh(context.Background(), live); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if gotPath != codearts.TokenPath {
		t.Fatalf("refresh path = %q, want %q (the WorkBuddy path would mean the dispatch is missing)", gotPath, codearts.TokenPath)
	}
	if strings.TrimSpace(gotDPoP) == "" {
		t.Fatal("no DPoP proof: STS rejects the refresh without it")
	}
	if !strings.Contains(gotForm, "grant_type=refresh_token") ||
		!strings.Contains(gotForm, "client_id="+codearts.ClientID) ||
		!strings.Contains(gotForm, "refresh_token=refresh-1") {
		t.Fatalf("refresh form = %q", gotForm)
	}

	rec := live.Record()
	if rec.AccessToken != "STS-NEW" || rec.RefreshToken != "refresh-2" {
		t.Fatalf("record after refresh = token %q refresh %q", rec.AccessToken, rec.RefreshToken)
	}
	if want := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC).Unix(); rec.ExpiresAt != want {
		t.Fatalf("expiry = %d, want %d", rec.ExpiresAt, want)
	}
	cred := codearts.CredentialOf(rec)
	if cred.AccessKeyID != "AK-NEW" || cred.SecretAccessKey != "SK-NEW" || cred.SecurityToken != "STS-NEW" {
		t.Fatalf("credential after refresh = %+v", cred)
	}
	if codearts.DPoPKeyOf(rec)["d"] == "" {
		t.Fatal("the DPoP key must survive a refresh")
	}
	if saved == nil || saved.AccessToken != "STS-NEW" {
		t.Fatalf("persist hook got %+v", saved)
	}
}

// A terminal refresh failure must surface as ErrNeedsLogin (so the scanner stops
// retrying every minute) and must not touch the stored credential.
func TestCodeArtsRefreshTerminalFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant","error_code":"InvalidDPoPHeader","error_msg":"dpop mismatch"}`)
	}))
	defer srv.Close()

	jwk, _ := codearts.NewDPoPKey()
	p := pool.New(nil)
	p.Add(codeartsRecord(t, jwk))
	live := p.Get("hw-1")

	r := &Refresher{
		Pool:    p,
		Up:      upstream.NewManager(upstream.DefaultTimeouts()),
		BaseURL: func(*pool.Account) string { return srv.URL },
	}
	err := r.Refresh(context.Background(), live)
	if !errors.Is(err, ErrNeedsLogin) {
		t.Fatalf("err = %v, want ErrNeedsLogin", err)
	}
	if rec := live.Record(); rec.AccessToken != "STS-OLD" || rec.RefreshToken != "refresh-1" {
		t.Fatalf("a failed refresh mutated the credential: %+v", rec)
	}
}

// A CodeArts credential with no expiry (imported or hand-written) used to be
// skipped forever: due() returned false for exp == 0. It now tries once and
// then backs off like a missing token.
func TestDueRefreshesCodeArtsWithoutExpiry(t *testing.T) {
	p := pool.New(nil)
	p.Add(&accounts.Record{UID: "hw-1", Realm: realm.CodeArts, AccessToken: "t", RefreshToken: "r"})
	r := &Refresher{Pool: p}
	acc := p.Get("hw-1")
	now := time.Now()
	if !r.due(acc, now) {
		t.Fatal("an unknown expiry on the Huawei channel must be refreshable")
	}
	r.mu.Lock()
	r.states = map[string]*state{"hw-1": {fails: 1, next: now.Add(time.Hour)}}
	r.mu.Unlock()
	if r.due(acc, now) {
		t.Fatal("after a failure the account must back off")
	}

	// WorkBuddy keeps the old rule: a missing expiry is not treated as due.
	p2 := pool.New(nil)
	p2.Add(&accounts.Record{UID: "cn-1", Realm: realm.CN, AccessToken: "t", RefreshToken: "r"})
	if (&Refresher{Pool: p2}).due(p2.Get("cn-1"), now) {
		t.Fatal("WorkBuddy behaviour must not change")
	}
}

func TestCodeArtsRefreshRateLimitSeparatesRejectedAccess(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "90")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error_code":"APIGW.0308","error_msg":"rate limited"}`)
	}))
	defer srv.Close()
	jwk, _ := codearts.NewDPoPKey()
	now := time.Now()
	rec := codeartsRecord(t, jwk)
	rec.ExpiresAt = now.Add(time.Hour).Unix()
	p := pool.New([]*accounts.Record{rec})
	a := p.Get(rec.UID)
	dir := t.TempDir()
	r := &Refresher{Pool: p, BaseURL: func(*pool.Account) string { return srv.URL },
		Persist: func(next *accounts.Record) error { _, err := accounts.Save(dir, next); return err }}

	err := r.Refresh(t.Context(), a)
	next, ok := RetryTime(err)
	if !ok || next.Before(now.Add(89*time.Second)) || !a.Ready("", now) {
		t.Fatalf("scheduled 429 must carry Retry-After and preserve the fresh AT: %v, %v", err, next)
	}
	if err := r.RefreshObserved(t.Context(), a, rec); a.Ready("", now) || errors.Is(err, ErrNeedsLogin) {
		t.Fatalf("explicit STS rejection must suspend the AT without marking the refresh token terminal: %v", err)
	}
	recs, loadErrs := accounts.Load(dir, now)
	if len(loadErrs) != 0 || len(recs) != 1 {
		t.Fatalf("reload: %d records, %v", len(recs), loadErrs)
	}
	restarted := pool.New(recs)
	b := restarted.Get(rec.UID)
	r2 := &Refresher{Pool: restarted, BaseURL: r.BaseURL}
	if _, ok := RetryTime(r2.Refresh(t.Context(), b)); !ok || b.Ready("", now) || calls.Load() != 1 {
		t.Fatalf("cooldown and access rejection must survive restart: %+v, calls=%d", b.Record().CodeArtsAuthState(), calls.Load())
	}
}

func TestCodeArtsRefreshReconcilesFailedPersistenceWithoutReplaying(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprintf(w, `{"refresh_token":"rt-rotated","credentials":{
			"access_key_id":"AK-NEW","secret_access_key":"SK-NEW","security_token":"STS-NEW","expiration":%q}}`,
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}))
	defer srv.Close()
	jwk, _ := codearts.NewDPoPKey()
	rec := codeartsRecord(t, jwk)
	p := pool.New([]*accounts.Record{rec})
	a := p.Get(rec.UID)
	now := time.Now()
	failed := false
	r := &Refresher{
		Pool: p, Now: func() time.Time { return now }, BaseURL: func(*pool.Account) string { return srv.URL },
		Persist: func(next *accounts.Record) error {
			if next.RefreshToken != rec.RefreshToken && !failed {
				failed = true
				return errors.New("offline")
			}
			return nil
		},
	}
	if err := r.Refresh(t.Context(), a); err == nil || a.Record().RefreshToken != rec.RefreshToken {
		t.Fatalf("failed durable write must keep the old pool record: %v", err)
	}
	now = now.Add(time.Minute)
	if err := r.Refresh(t.Context(), a); err != nil || a.Record().RefreshToken != "rt-rotated" || calls.Load() != 1 {
		t.Fatalf("persist first without exchanging the old RT again: %v, calls=%d", err, calls.Load())
	}
	if err := r.RefreshObserved(t.Context(), a, rec); err != nil || !a.Ready("", now) || calls.Load() != 1 {
		t.Fatalf("a late rejection using the old AT must leave the replacement in service: %v, calls=%d", err, calls.Load())
	}
}
