package codearts

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestNewSessionBuildsAuthorizeURL(t *testing.T) {
	s, err := NewSession(0)
	if err != nil {
		t.Fatal(err)
	}
	if s.TicketID == "" || s.Secret == "" || s.Verifier == "" || s.Challenge == "" || s.DPoP["d"] == "" {
		t.Fatalf("session is incomplete: %+v", s)
	}
	u, err := url.Parse(s.URL())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.URL(), PortalHost+"/authorize?") {
		t.Fatalf("url = %s", s.URL())
	}
	q := u.Query()
	want := map[string]string{
		"client_id":             ClientID,
		"uri_scheme":            ClientID,
		"code_challenge":        s.Challenge,
		"code_challenge_method": "SHA-256",
		"ticket_id":             s.TicketID,
		"plugin-name":           PluginName,
		"plugin-version":        PluginVersion,
		"port":                  "0",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Fatalf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
}

func TestPKCE(t *testing.T) {
	v1, c1, err := PKCE()
	if err != nil {
		t.Fatal(err)
	}
	_, c2, _ := PKCE()
	if v1 == "" || c1 == "" || c1 == c2 {
		t.Fatal("PKCE must be random")
	}
}

// PollTicket returns (nil, nil) until the browser finishes: only a payload with
// an identity counts as done.
func TestPollTicketPending(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/snap-manager"+LoginTicket {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("ticket_id") != "tid" || r.URL.Query().Get("secret") != "sec" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		if r.Header.Get("plugin-name") != PluginName {
			t.Errorf("plugin-name = %q", r.Header.Get("plugin-name"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	tok, err := PollTicket(context.Background(), srv.Client(), srv.URL, "tid", "sec")
	if err != nil || tok != nil {
		t.Fatalf("pending = (%v, %v), want (nil, nil)", tok, err)
	}
}

func TestPollTicketComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user_id":"u-9","user_name":"hw","refresh_token":"r-9",
			"credentials":{"access_key_id":"AK9","secret_access_key":"SK9","security_token":"STS9",
			"expiration":"2026-12-01T00:00:00Z"}}`))
	}))
	defer srv.Close()
	tok, err := PollTicket(context.Background(), srv.Client(), srv.URL, "tid", "sec")
	if err != nil {
		t.Fatal(err)
	}
	if tok == nil || tok.UserID != "u-9" || tok.Credentials.SecurityToken != "STS9" || tok.RefreshToken != "r-9" {
		t.Fatalf("token = %+v", tok)
	}
}

// A finished login without credentials is an error, not a pending poll.
func TestPollTicketCompleteWithoutCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"user_name":"hw"}`))
	}))
	defer srv.Close()
	if _, err := PollTicket(context.Background(), srv.Client(), srv.URL, "tid", "sec"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestExchangeCodePostsPKCEForm(t *testing.T) {
	var form url.Values
	var sawDPoP string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != TokenPath {
			t.Errorf("path = %s", r.URL.Path)
		}
		sawDPoP = r.Header.Get("DPoP")
		_ = r.ParseForm()
		form = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user_id":"u-1","credentials":{"access_key_id":"AK","secret_access_key":"SK","security_token":"STS"}}`))
	}))
	defer srv.Close()

	jwk, _ := NewDPoPKey()
	tok, err := ExchangeCode(context.Background(), srv.Client(), srv.URL, "code-1", "verifier-1", 4567, jwk)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Credentials.SecurityToken != "STS" {
		t.Fatalf("token = %+v", tok)
	}
	if sawDPoP == "" {
		t.Fatal("no DPoP proof")
	}
	want := map[string]string{
		"client_id":     ClientID,
		"code":          "code-1",
		"code_verifier": "verifier-1",
		"grant_type":    "authorization_code",
		"redirect_uri":  "http://127.0.0.1:4567" + CallbackPath,
	}
	for k, v := range want {
		if form.Get(k) != v {
			t.Fatalf("%s = %q, want %q", k, form.Get(k), v)
		}
	}
}

func TestCurrentUserReadsIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != CurrentUserPath {
			t.Errorf("path = %s", r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "SDK-HMAC-SHA256 Access=AK") {
			t.Errorf("unsigned request: %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"user_id":"u-1","user_name":"hw","domain_id":"d-1"}`))
	}))
	defer srv.Close()
	uid, name, domain, err := CurrentUser(context.Background(), srv.Client(), srv.URL, CreditCredential())
	if err != nil {
		t.Fatal(err)
	}
	if uid != "u-1" || name != "hw" || domain != "d-1" {
		t.Fatalf("identity = %s/%s/%s", uid, name, domain)
	}
}

// A business error in the ticket response is an error, not "still waiting".
func TestPollTicketBusinessError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error_code":"APIG.0201","error_msg":"ticket not found"}`))
	}))
	defer srv.Close()
	_, err := PollTicket(context.Background(), srv.Client(), srv.URL, "tid", "sec")
	if err == nil || !strings.Contains(err.Error(), "APIG.0201") {
		t.Fatalf("err = %v", err)
	}
}
