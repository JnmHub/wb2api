package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"wb2api/internal/codeartsync"
	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream/codearts"
)

// ticketServer emulates snap-manager: it answers "pending" until authorized.
func ticketServer(t *testing.T, done *atomic.Bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/snap-manager"+codearts.LoginTicket:
			if !done.Load() {
				fmt.Fprint(w, `{}`)
				return
			}
			fmt.Fprint(w, `{"user_id":"hw-9","user_name":"华为用户","refresh_token":"r-9",
				"credentials":{"access_key_id":"AK9","secret_access_key":"SK9","security_token":"STS9",
				"expiration":"2026-12-01T00:00:00Z"}}`)
		case r.URL.Path == codearts.AgentListPath:
			fmt.Fprint(w, `{"agents":[{"agent_id":"a-1","agent_name":"CodeAgent","show_in_ide":true,"alias":{"alias_zh_cn":"智能体"}}]}`)
		case r.URL.Path == codearts.AgentDetailPath:
			fmt.Fprint(w, `{"gpts":{"models":[{"model_name":"GLM-5.2"}]}}`)
		case r.URL.Path == codearts.BuiltinPath:
			fmt.Fprint(w, `{"builtinModels":[{"model_id":"Qwen3-VL-235B"}]}`)
		case r.URL.Path == codearts.BenefitClaimPath:
			fmt.Fprint(w, `{"error_code":"0000"}`)
		case r.URL.Path == codearts.BenefitConfigPath:
			fmt.Fprint(w, `{"error_code":"0000","result":{"models":[{"model_id":"deepseek-v4.1-flash-1017"}]}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The server-side login needs no inbound callback: the panel shows the portal
// URL and polls the ticket until the browser finishes.
func TestCodeArtsLoginFlow(t *testing.T) {
	var done atomic.Bool
	srv := ticketServer(t, &done)
	p := pool.New(nil)
	var saved *accounts.Record
	o := &Operations{
		Pool:              p,
		Persist:           func(r *accounts.Record) error { saved = r; return nil },
		CodeArtsLoginBase: srv.URL,
	}

	start, err := o.startCodeArtsLogin()
	if err != nil {
		t.Fatalf("startCodeArtsLogin: %v", err)
	}
	state, _ := start["state"].(string)
	if state == "" {
		t.Fatalf("start = %#v", start)
	}
	authURL, _ := start["authUrl"].(string)
	u, err := url.Parse(authURL)
	if err != nil || !strings.Contains(authURL, codearts.PortalHost) {
		t.Fatalf("authUrl = %q", authURL)
	}
	if u.Query().Get("code_challenge") == "" || u.Query().Get("ticket_id") == "" {
		t.Fatalf("authUrl lost the PKCE/ticket parameters: %s", authURL)
	}
	if start["realm"] != realm.CodeArts {
		t.Fatalf("realm = %#v", start["realm"])
	}

	// Pending while the user is in the browser.
	if got := o.pollCodeArtsLogin(context.Background(), state); got["status"] != "pending" {
		t.Fatalf("pending poll = %#v", got)
	}

	done.Store(true)
	got := o.pollCodeArtsLogin(context.Background(), state)
	if got["status"] != "ok" || got["uid"] != "hw-9" {
		t.Fatalf("finished poll = %#v", got)
	}
	if saved == nil {
		t.Fatal("the credential was not persisted")
	}
	rec := p.Get("hw-9")
	if rec == nil {
		t.Fatal("the account was not added to the pool")
	}
	if saved.Realm != realm.CodeArts || saved.AccessToken != "STS9" || saved.RefreshToken != "r-9" {
		t.Fatalf("record = %+v", saved)
	}
	cred := codearts.CredentialOf(saved)
	if cred.AccessKeyID != "AK9" || cred.SecretAccessKey != "SK9" {
		t.Fatalf("credential = %+v", cred)
	}
	// The DPoP key and PKCE verifier must survive: without them the credential
	// can never be refreshed.
	if codearts.DPoPKeyOf(saved)["d"] == "" {
		t.Fatal("dpop key not stored")
	}
	if codearts.CodeVerifierOf(saved) == "" {
		t.Fatal("code_verifier not stored")
	}
	// A finished state is gone; a second poll is unknown.
	if again := o.pollCodeArtsLogin(context.Background(), state); again["status"] != "unknown" {
		t.Fatalf("second poll = %#v", again)
	}
}

func TestCodeArtsLoginPollUnknownState(t *testing.T) {
	o := &Operations{Pool: pool.New(nil)}
	if got := o.pollCodeArtsLogin(context.Background(), "nope"); got["status"] != "unknown" {
		t.Fatalf("got %#v", got)
	}
}

// login/start must route on the realm: codearts never touches the WorkBuddy
// auth endpoint.
func TestLoginStartRoutesCodeArts(t *testing.T) {
	o := &Operations{Pool: pool.New(nil)}
	mux := http.NewServeMux()
	o.Register(mux, func(h http.Handler) http.Handler { return h })
	req := httptest.NewRequest(http.MethodPost, "/api/v2/accounts/login/start",
		strings.NewReader(`{"realm":"codearts"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["authUrl"].(string); !ok {
		t.Fatalf("no authUrl: %#v", out)
	}
}

// The panel's model refresh re-runs discovery and reports what it found.
func TestCodeArtsModelsRefreshEndpoint(t *testing.T) {
	var done atomic.Bool
	done.Store(true)
	srv := ticketServer(t, &done)
	p := pool.New([]*accounts.Record{codeartsAccountRec("hw-1")})
	svc := &codeartsync.Service{Pool: p, Base: srv.URL, BenefitBase: srv.URL, ClaimBenefit: true}
	o := &Operations{Pool: p, CodeArts: svc}
	mux := http.NewServeMux()
	o.Register(mux, func(h http.Handler) http.Handler { return h })

	req := httptest.NewRequest(http.MethodPost, "/api/v2/codearts/models/refresh", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	var out struct {
		OK       bool `json:"ok"`
		Count    int  `json:"count"`
		Accounts []struct {
			UID    string   `json:"uid"`
			OK     bool     `json:"ok"`
			Models []string `json:"models"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body: %v (%s)", err, w.Body.String())
	}
	if !out.OK || out.Count != 1 || len(out.Accounts) != 1 || !out.Accounts[0].OK {
		t.Fatalf("out = %+v", out)
	}
	if len(out.Accounts[0].Models) != 3 {
		t.Fatalf("models = %v", out.Accounts[0].Models)
	}
	// The catalogue is on the record now, so /v1/models can serve it.
	if models := codearts.ModelsOf(p.Get("hw-1").Record()); len(models) != 3 {
		t.Fatalf("stored models = %v", models)
	}
}

// A disabled channel answers 501 instead of panicking.
func TestCodeArtsModelsRefreshWithoutChannel(t *testing.T) {
	o := &Operations{Pool: pool.New(nil)}
	mux := http.NewServeMux()
	o.Register(mux, func(h http.Handler) http.Handler { return h })
	req := httptest.NewRequest(http.MethodPost, "/api/v2/codearts/models/refresh", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d", w.Code)
	}
}

// The PKCE challenge must be the S256 of the verifier that is stored later.
func TestCodeArtsLoginPKCEIsConsistent(t *testing.T) {
	sess, err := codearts.NewSession(0)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(sess.Verifier))
	if got := base64.RawURLEncoding.EncodeToString(sum[:]); got != sess.Challenge {
		t.Fatalf("challenge = %s, want %s", sess.Challenge, got)
	}
}
