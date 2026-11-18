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

// ticketServer emulates snap-manager: the ticket stays "invalid" until the
// portal-delivered secret is used, mirroring the real behaviour.
type ticketServer struct {
	done      atomic.Bool
	portalSec atomic.Value // string
	sawSecret atomic.Value
	exchanged atomic.Value // url.Values
}

func (t *ticketServer) handler(tb *testing.T) http.HandlerFunc {
	tb.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/snap-manager"+codearts.LoginTicket:
			secret := r.URL.Query().Get("secret")
			t.sawSecret.Store(secret)
			want, _ := t.portalSec.Load().(string)
			if want == "" || secret != want {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error_code":"TM.00001001","error_msg":"无效 ticketId: 124a2da9"}`)
				return
			}
			if !t.done.Load() {
				fmt.Fprint(w, `{}`)
				return
			}
			fmt.Fprint(w, `{"user_id":"hw-9","user_name":"华为用户","refresh_token":"r-9",
				"credentials":{"access_key_id":"AK9","secret_access_key":"SK9","security_token":"STS9",
				"expiration":"2026-12-01T00:00:00Z"}}`)
		case r.URL.Path == codearts.TokenPath:
			_ = r.ParseForm()
			t.exchanged.Store(r.PostForm)
			fmt.Fprint(w, `{"user_id":"hw-9","user_name":"华为用户","refresh_token":"r-9",
				"credentials":{"access_key_id":"AK9","secret_access_key":"SK9","security_token":"STS9",
				"expiration":"2026-12-01T00:00:00Z"}}`)
		case r.URL.Path == codearts.AgentListPath:
			fmt.Fprint(w, `{"agents":[{"agent_id":"a-1","agent_name":"CodeAgent","show_in_ide":true,"alias":{"alias_zh_cn":"智能体"}}]}`)
		case r.URL.Path == codearts.AgentDetailPath:
			fmt.Fprint(w, `{"gpts":{"models":[{"model_name":"GLM-5.2"}]}}`)
		case r.URL.Path == codearts.BuiltinPath:
			fmt.Fprint(w, `{"builtinModels":[{"model_id":"Qwen3-VL-235B"}]}`)
		case r.URL.Path == codearts.BenefitGatePath:
			fmt.Fprint(w, `{"enabled":true}`)
		case r.URL.Path == codearts.BenefitClaimPath:
			fmt.Fprint(w, `{"error_code":"0000"}`)
		case r.URL.Path == codearts.BenefitConfigPath:
			fmt.Fprint(w, `{"error_code":"0000","result":{"models":[{"model_id":"deepseek-v4.1-flash-1017"}]}}`)
		default:
			tb.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}
}

func newLoginOps(t *testing.T, ts *ticketServer, srv *httptest.Server) (*Operations, *[]*accounts.Record) {
	t.Helper()
	p := pool.New(nil)
	saved := []*accounts.Record{}
	o := &Operations{
		Pool:              p,
		Persist:           func(r *accounts.Record) error { saved = append(saved, r); return nil },
		CodeArtsLoginBase: srv.URL,
		CodeArtsSTSBase:   srv.URL,
	}
	return o, &saved
}

// The browser login needs the portal's own secret: polling with the locally
// generated one is answered with TM.00001001 ("invalid ticketId"), which is a
// waiting state, not a failure.
func TestCodeArtsLoginPollIsPendingUntilThePortalSecretArrives(t *testing.T) {
	ts := &ticketServer{done: atomic.Bool{}}
	srv := httptest.NewServer(ts.handler(t))
	defer srv.Close()
	o, _ := newLoginOps(t, ts, srv)

	req := httptest.NewRequest(http.MethodPost, "/api/v2/accounts/login/start", strings.NewReader(`{"realm":"codearts"}`))
	req.Host = "127.0.0.1:8788"
	start, err := o.startCodeArtsLogin(req)
	if err != nil {
		t.Fatal(err)
	}
	state := start["state"].(string)
	cbURL, _ := start["callback"].(string)
	if !strings.HasPrefix(cbURL, "http://127.0.0.1:") || !strings.HasSuffix(cbURL, codearts.CallbackPath) {
		t.Fatalf("callback = %v", start["callback"])
	}
	u, _ := url.Parse(start["authUrl"].(string))
	// Each attempt gets its own loopback port so the code/verifier pair cannot
	// be mixed up with another tab's login.
	if p := u.Query().Get("port"); p == "" || p == "0" || !strings.Contains(cbURL, ":"+p) {
		t.Fatalf("authorize port = %q, callback = %q", p, cbURL)
	}

	got := o.pollCodeArtsLogin(context.Background(), state)
	if got["status"] != "pending" {
		t.Fatalf("poll before the secret = %#v, want pending", got)
	}
	if !strings.Contains(fmt.Sprint(got["message"]), "等待") {
		t.Fatalf("message = %v", got["message"])
	}

	// First callback: the portal hands over its secret and asks for a redirect.
	rec := httptest.NewRecorder()
	cb := httptest.NewRequest(http.MethodGet, "/oauth/callback?secret=PORTAL-SECRET&redirect=https%3A%2F%2Fportal.example%2Fnext", nil)
	o.codeartsCallback(rec, cb)
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("callback status = %d body = %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "https://portal.example/next" {
		t.Fatalf("redirect = %q", loc)
	}

	// The portal has now registered that secret; the poll uses it and the
	// ticket becomes valid.
	ts.portalSec.Store("PORTAL-SECRET")
	got = o.pollCodeArtsLogin(context.Background(), state)
	if got["status"] != "pending" {
		t.Fatalf("poll after the secret = %#v", got)
	}
	if saw, _ := ts.sawSecret.Load().(string); saw != "PORTAL-SECRET" {
		t.Fatalf("poll secret = %q, want the portal one", saw)
	}

	ts.done.Store(true)
	got = o.pollCodeArtsLogin(context.Background(), state)
	if got["status"] != "ok" || got["uid"] != "hw-9" {
		t.Fatalf("finished poll = %#v", got)
	}
}

// The second callback carries the authorization code and finishes the login.
func TestCodeArtsLoginCallbackExchangesCode(t *testing.T) {
	ts := &ticketServer{}
	ts.portalSec.Store("PORTAL-SECRET")
	srv := httptest.NewServer(ts.handler(t))
	defer srv.Close()
	o, saved := newLoginOps(t, ts, srv)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Host = "127.0.0.1:8788"
	start, err := o.startCodeArtsLogin(req)
	if err != nil {
		t.Fatal(err)
	}
	state := start["state"].(string)

	rec := httptest.NewRecorder()
	cb := httptest.NewRequest(http.MethodGet, "/oauth/callback?code=code-1&secret=PORTAL-SECRET", nil)
	o.codeartsCallback(rec, cb)
	if rec.Code != http.StatusOK {
		t.Fatalf("callback status = %d body = %s", rec.Code, rec.Body.String())
	}
	form, _ := ts.exchanged.Load().(url.Values)
	if form == nil {
		t.Fatal("the code was never exchanged")
	}
	if form.Get("grant_type") != "authorization_code" || form.Get("code") != "code-1" {
		t.Fatalf("exchange form = %v", form)
	}
	if codearts.TokenPath != "/v1/oauth2/tokens" {
		t.Fatalf("token path = %q, want the STS endpoint", codearts.TokenPath)
	}
	if want, _ := o.codeartsLogins.get(state); want != nil {
		if got := form.Get("redirect_uri"); !strings.HasSuffix(got, codearts.CallbackPath) ||
			!strings.Contains(got, fmt.Sprint(want.session.Port)) {
			t.Fatalf("redirect_uri = %q, want the attempt port %d", got, want.session.Port)
		}
	}
	if len(*saved) != 1 {
		t.Fatalf("saved %d records", len(*saved))
	}
	acc := o.Pool.Get("hw-9")
	if acc == nil {
		t.Fatal("the account was not added")
	}
	if rec := acc.Record(); rec.Realm != realm.CodeArts || codearts.DPoPKeyOf(rec)["d"] == "" {
		t.Fatalf("record = %+v", rec)
	}
	got := o.pollCodeArtsLogin(context.Background(), state)
	if got["status"] != "ok" {
		t.Fatalf("poll after the callback = %#v", got)
	}
}

// Without a running login the callback is refused instead of writing an account.
func TestCodeArtsCallbackWithoutLogin(t *testing.T) {
	o := &Operations{Pool: pool.New(nil)}
	rec := httptest.NewRecorder()
	o.codeartsCallback(rec, httptest.NewRequest(http.MethodGet, "/oauth/callback?code=x", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

// The route has to be reachable without the panel session, otherwise the
// browser callback cannot land.
func TestCodeArtsCallbackIsUnauthenticated(t *testing.T) {
	o := &Operations{Pool: pool.New(nil)}
	mux := http.NewServeMux()
	o.Register(mux, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "panel auth must not run here", http.StatusUnauthorized)
		})
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/oauth/callback?code=x", nil))
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("the callback is behind panel authentication")
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
	req.Host = "127.0.0.1:8788"
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

func TestCodeArtsLoginPollUnknownState(t *testing.T) {
	o := &Operations{Pool: pool.New(nil)}
	if got := o.pollCodeArtsLogin(context.Background(), "nope"); got["status"] != "unknown" {
		t.Fatalf("got %#v", got)
	}
}

// The panel's model refresh re-runs discovery and reports what it found.
func TestCodeArtsModelsRefreshEndpoint(t *testing.T) {
	ts := &ticketServer{}
	srv := httptest.NewServer(ts.handler(t))
	defer srv.Close()
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
	if models := codearts.ModelsOf(p.Get("hw-1").Record()); len(models) != 3 {
		t.Fatalf("stored models = %v", models)
	}
}

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
	sess, err := codearts.NewSession(8788)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(sess.Verifier))
	if got := base64.RawURLEncoding.EncodeToString(sum[:]); got != sess.Challenge {
		t.Fatalf("challenge = %s, want %s", sess.Challenge, got)
	}
}

// The token endpoint can answer with credentials only. The account id then has
// to come from snap-manager's current/user with the fresh STS token.
func TestCodeArtsLoginResolvesIdentityWhenMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case codearts.TokenPath:
			fmt.Fprint(w, `{"refresh_token":"r-9","credentials":{"access_key_id":"AK9",
				"secret_access_key":"SK9","security_token":"STS9","expiration":"2026-12-01T00:00:00Z"}}`)
		case codearts.CurrentUserPath:
			if !strings.HasPrefix(r.Header.Get("Authorization"), "SDK-HMAC-SHA256 Access=AK9") {
				t.Errorf("current/user not signed with the new AK: %q", r.Header.Get("Authorization"))
			}
			fmt.Fprint(w, `{"user_id":"hw-77","user_name":"华为用户","domain_id":"d"}`)
		case codearts.AgentListPath:
			fmt.Fprint(w, `{"agents":[{"agent_id":"a-1","agent_name":"CodeAgent","show_in_ide":true,"alias":{"alias_zh_cn":"智能体"}}]}`)
		case codearts.AgentDetailPath:
			fmt.Fprint(w, `{"gpts":{"models":[{"model_name":"GLM-5.2"}]}}`)
		case codearts.BuiltinPath:
			fmt.Fprint(w, `{"builtinModels":[]}`)
		case codearts.BenefitGatePath:
			fmt.Fprint(w, `{"enabled":true}`)
		case codearts.BenefitClaimPath, codearts.BenefitConfigPath:
			fmt.Fprint(w, `{"error_code":"0000","result":{"models":[]}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	o := &Operations{
		Pool:              pool.New(nil),
		CodeArtsLoginBase: srv.URL,
		CodeArtsSTSBase:   srv.URL,
		Persist:           func(*accounts.Record) error { return nil },
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Host = "127.0.0.1:8788"
	if _, err := o.startCodeArtsLogin(req); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	o.codeartsCallback(rec, httptest.NewRequest(http.MethodGet, "/oauth/callback?code=c&secret=s", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("callback status = %d body = %s", rec.Code, rec.Body.String())
	}
	acc := o.Pool.Get("hw-77")
	if acc == nil {
		t.Fatal("the account was not created from the resolved identity")
	}
	if acc.Record().Nickname != "华为用户" {
		t.Fatalf("nickname = %q", acc.Record().Nickname)
	}
}
