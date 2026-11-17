package refresh

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
