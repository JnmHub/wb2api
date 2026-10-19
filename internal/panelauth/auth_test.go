package panelauth

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/store/settings"
)

type clock struct{ ns atomic.Int64 }

func (c *clock) now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *clock) advance(d time.Duration) { c.ns.Add(int64(d)) }

type rig struct {
	t     *testing.T
	dir   string
	clk   *clock
	set   *settings.Store
	auth  *Auth
	mux   *http.ServeMux
	calls atomic.Int32
}

// open (re)creates the whole auth stack on dir, as a gateway restart would.
func open(t *testing.T, dir string, clk *clock) *rig {
	t.Helper()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	st.Now = clk.now
	set := settings.Open(dir)
	r := &rig{t: t, dir: dir, clk: clk, set: set, mux: http.NewServeMux()}
	r.auth = &Auth{Store: st, Pass: set, Proxies: ParseTrustedProxies("10.0.0.0/8"), FailDelay: time.Millisecond, Now: clk.now}
	r.auth.Register(r.mux, "/api/v2/session")
	r.mux.Handle("/api/v2/protected", r.auth.Require(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.calls.Add(1)
		io.WriteString(w, "secret")
	})))
	return r
}

func newRig(t *testing.T) *rig {
	clk := &clock{}
	clk.ns.Store(time.Now().UnixNano())
	r := open(t, t.TempDir(), clk)
	if err := r.set.SetPanelPassword("hunter2!"); err != nil {
		t.Fatal(err)
	}
	return r
}

type resp struct {
	code int
	body string
	hdr  http.Header
	jar  []*http.Cookie
}

func (r *rig) do(method, path, body string, hdr map[string]string, cookies ...*http.Cookie) resp {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "203.0.113.9:5555"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	r.mux.ServeHTTP(w, req)
	res := w.Result()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, string(b), res.Header, res.Cookies()}
}

func cookieOf(t *testing.T, rs resp) *http.Cookie {
	t.Helper()
	for _, c := range rs.jar {
		if c.Name == CookieName && c.MaxAge >= 0 && c.Value != "" {
			return c
		}
	}
	t.Fatalf("no session cookie in %+v", rs.jar)
	return nil
}

func (r *rig) login(remember bool) (*http.Cookie, string) {
	rs := r.do("POST", "/api/v2/session/login", `{"password":"hunter2!","remember":`+map[bool]string{true: "true", false: "false"}[remember]+`}`, nil)
	if rs.code != 200 {
		r.t.Fatalf("login failed: %d %s", rs.code, rs.body)
	}
	var out struct {
		CSRF string `json:"csrf"`
	}
	_ = json.Unmarshal([]byte(rs.body), &out)
	return cookieOf(r.t, rs), out.CSRF
}

func TestLoginSetsHardenedCookie(t *testing.T) {
	r := newRig(t)
	rs := r.do("POST", "/api/v2/session/login", `{"password":"hunter2!"}`, nil)
	c := cookieOf(t, rs)
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Secure {
		t.Fatalf("cookie flags: %+v", c)
	}
	if c.MaxAge < int((29 * 24 * time.Hour).Seconds()) {
		t.Fatalf("a remembered login should last ~30 days, MaxAge=%d", c.MaxAge)
	}
	if strings.Contains(rs.body, c.Value) {
		t.Fatal("the token must only travel in the cookie, never in the body")
	}
	// behind a trusted TLS-terminating proxy the cookie becomes Secure
	req := httptest.NewRequest("POST", "/api/v2/session/login", strings.NewReader(`{"password":"hunter2!"}`))
	req.RemoteAddr = "10.1.2.3:4000"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	r.mux.ServeHTTP(w, req)
	secure := false
	for _, ck := range w.Result().Cookies() {
		secure = secure || (ck.Name == CookieName && ck.Secure)
	}
	if !secure {
		t.Fatal("cookie must be Secure behind an https proxy")
	}
	// an untrusted peer cannot claim https
	req2 := httptest.NewRequest("POST", "/api/v2/session/login", strings.NewReader(`{"password":"hunter2!"}`))
	req2.RemoteAddr = "203.0.113.9:4000"
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Forwarded-Proto", "https")
	w2 := httptest.NewRecorder()
	r.mux.ServeHTTP(w2, req2)
	for _, ck := range w2.Result().Cookies() {
		if ck.Secure {
			t.Fatal("X-Forwarded-Proto from an untrusted peer must be ignored")
		}
	}
}

// The headline requirement: the login survives a gateway restart.
func TestSessionSurvivesRestartAndStoresOnlyHashes(t *testing.T) {
	r := newRig(t)
	cookie, _ := r.login(true)
	if rs := r.do("GET", "/api/v2/protected", "", nil, cookie); rs.code != 200 {
		t.Fatalf("before restart: %d", rs.code)
	}
	if err := r.auth.Store.Close(); err != nil {
		t.Fatal(err)
	}

	r2 := open(t, r.dir, r.clk) // a brand new process on the same directory
	if rs := r2.do("GET", "/api/v2/protected", "", nil, cookie); rs.code != 200 || rs.body != "secret" {
		t.Fatalf("after restart the browser must still be logged in: %d %s", rs.code, rs.body)
	}
	if rs := r2.do("GET", "/api/v2/session", "", nil, cookie); rs.code != 200 || !strings.Contains(rs.body, `"authenticated":true`) {
		t.Fatalf("session status after restart: %d %s", rs.code, rs.body)
	}
	raw, err := os.ReadFile(filepath.Join(r.dir, SessionFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), cookie.Value) {
		t.Fatal("the state file must not contain the raw session token")
	}
	if !strings.Contains(string(raw), hashToken(cookie.Value)) {
		t.Fatal("the state file should hold the token hash")
	}
}

func TestExpiryIsSliding(t *testing.T) {
	r := newRig(t)
	cookie, _ := r.login(false) // 12 h idle lifetime
	for i := 0; i < 5; i++ {
		r.clk.advance(9 * time.Hour) // active every 9 h: stays logged in far beyond 12 h in total
		if rs := r.do("GET", "/api/v2/protected", "", nil, cookie); rs.code != 200 {
			t.Fatalf("active session expired after %d x 9h", i+1)
		}
	}
	r.clk.advance(13 * time.Hour) // idle longer than the lifetime
	if rs := r.do("GET", "/api/v2/protected", "", nil, cookie); rs.code != 401 {
		t.Fatalf("an idle session must expire, got %d", rs.code)
	}
}

func TestCookieIsReissuedWhenTheSessionSlides(t *testing.T) {
	r := newRig(t)
	cookie, _ := r.login(true)
	r.clk.advance(2 * time.Hour)
	rs := r.do("GET", "/api/v2/protected", "", nil, cookie)
	var fresh *http.Cookie
	for _, c := range rs.jar {
		if c.Name == CookieName {
			fresh = c
		}
	}
	if fresh == nil || fresh.MaxAge < int((29*24*time.Hour).Seconds()) {
		t.Fatalf("the cookie lifetime must slide with the session: %+v", fresh)
	}
}

func TestWrongPasswordAndRateLimit(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 5; i++ {
		if rs := r.do("POST", "/api/v2/session/login", `{"password":"nope"}`, nil); rs.code != 401 {
			t.Fatalf("attempt %d: %d", i, rs.code)
		}
	}
	rs := r.do("POST", "/api/v2/session/login", `{"password":"hunter2!"}`, nil)
	if rs.code != 429 || rs.hdr.Get("Retry-After") == "" {
		t.Fatalf("the sixth attempt inside a minute must be refused, even with the right password: %d", rs.code)
	}
	r.clk.advance(61 * time.Second)
	if rs := r.do("POST", "/api/v2/session/login", `{"password":"hunter2!"}`, nil); rs.code != 200 {
		t.Fatalf("limit must lift after the window: %d", rs.code)
	}
}

func TestRateLimitBucketsUseTheRealClientOnlyBehindTrustedProxies(t *testing.T) {
	r := newRig(t)
	post := func(peer, xff string) int {
		req := httptest.NewRequest("POST", "/api/v2/session/login", strings.NewReader(`{"password":"bad"}`))
		req.RemoteAddr = peer + ":1"
		req.Header.Set("Content-Type", "application/json")
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		w := httptest.NewRecorder()
		r.mux.ServeHTTP(w, req)
		return w.Code
	}
	// Behind the trusted proxy 10.0.0.1, client A exhausts its own bucket...
	for i := 0; i < 5; i++ {
		post("10.0.0.1", "198.51.100.1")
	}
	if post("10.0.0.1", "198.51.100.1") != 429 {
		t.Fatal("client A should be limited")
	}
	// ...but client B behind the same proxy is not locked out with it.
	if post("10.0.0.1", "198.51.100.2") != 401 {
		t.Fatal("client B must have its own bucket")
	}
	// An untrusted peer cannot dodge the limit by forging X-Forwarded-For.
	for i := 0; i < 5; i++ {
		post("203.0.113.50", "1.1.1."+string(rune('0'+i)))
	}
	if post("203.0.113.50", "9.9.9.9") != 429 {
		t.Fatal("forged forwarding headers from an untrusted peer must not change the bucket")
	}
}

func TestCSRFAndContentTypeGuards(t *testing.T) {
	r := newRig(t)
	cookie, csrf := r.login(true)
	if rs := r.do("POST", "/api/v2/protected", `{}`, nil, cookie); rs.code != 403 || !strings.Contains(rs.body, "csrf") {
		t.Fatalf("a cookie-authenticated write without the CSRF header: %d %s", rs.code, rs.body)
	}
	if rs := r.do("POST", "/api/v2/protected", `{}`, map[string]string{CSRFHeader: "wrong"}, cookie); rs.code != 403 {
		t.Fatalf("wrong CSRF token: %d", rs.code)
	}
	if rs := r.do("POST", "/api/v2/protected", `{}`, map[string]string{CSRFHeader: csrf}, cookie); rs.code != 200 {
		t.Fatalf("correct CSRF token: %d %s", rs.code, rs.body)
	}
	if rs := r.do("POST", "/api/v2/protected", `a=b`, map[string]string{CSRFHeader: csrf, "Content-Type": "application/x-www-form-urlencoded"}, cookie); rs.code != 415 {
		t.Fatalf("form posts must be refused: %d", rs.code)
	}
	if rs := r.do("GET", "/api/v2/protected", "", nil, cookie); rs.code != 200 {
		t.Fatal("reads need no CSRF token")
	}
}

func TestLegacyHeaderTokenWorksWithoutCSRF(t *testing.T) {
	r := newRig(t)
	cookie, _ := r.login(true)
	if rs := r.do("POST", "/api/v2/protected", `{}`, map[string]string{LegacyTokenHeader: cookie.Value}); rs.code != 200 {
		t.Fatalf("the legacy header is not ambient, so it needs no CSRF token: %d", rs.code)
	}
	if rs := r.do("GET", "/api/v2/protected", "", map[string]string{LegacyTokenHeader: "bogus"}); rs.code != 401 {
		t.Fatalf("bogus legacy token: %d", rs.code)
	}
}

func TestUnauthenticatedAccess(t *testing.T) {
	r := newRig(t)
	if rs := r.do("GET", "/api/v2/protected", "", nil); rs.code != 401 {
		t.Fatalf("%d", rs.code)
	}
	rs := r.do("GET", "/api/v2/session", "", nil)
	if rs.code != 401 || !strings.Contains(rs.body, `"authenticated":false`) {
		t.Fatalf("%d %s", rs.code, rs.body)
	}
	if r.calls.Load() != 0 {
		t.Fatal("handler must not run")
	}
}

func TestLogoutRevokesAndClearsCookie(t *testing.T) {
	r := newRig(t)
	cookie, csrf := r.login(true)
	if rs := r.do("POST", "/api/v2/session/logout", `{}`, nil, cookie); rs.code != 403 {
		t.Fatalf("logout without CSRF would let any site log the admin out: %d", rs.code)
	}
	rs := r.do("POST", "/api/v2/session/logout", `{}`, map[string]string{CSRFHeader: csrf}, cookie)
	if rs.code != 200 {
		t.Fatalf("%d", rs.code)
	}
	cleared := false
	for _, c := range rs.jar {
		cleared = cleared || (c.Name == CookieName && c.MaxAge < 0)
	}
	if !cleared {
		t.Fatal("cookie not cleared")
	}
	if rs := r.do("GET", "/api/v2/protected", "", nil, cookie); rs.code != 401 {
		t.Fatalf("revoked session must stop working: %d", rs.code)
	}
}

func TestPasswordChangeRevokesOtherDevices(t *testing.T) {
	r := newRig(t)
	a, csrfA := r.login(true)
	b, _ := r.login(true)
	rs := r.do("POST", "/api/v2/session/password", `{"current":"wrong","new":"s3cret-new"}`, map[string]string{CSRFHeader: csrfA}, a)
	if rs.code != 401 {
		t.Fatalf("wrong current password: %d", rs.code)
	}
	rs = r.do("POST", "/api/v2/session/password", `{"current":"hunter2!","new":"abc"}`, map[string]string{CSRFHeader: csrfA}, a)
	if rs.code != 400 {
		t.Fatalf("too short: %d", rs.code)
	}
	rs = r.do("POST", "/api/v2/session/password", `{"current":"hunter2!","new":"s3cret-new"}`, map[string]string{CSRFHeader: csrfA}, a)
	if rs.code != 200 || !strings.Contains(rs.body, `"revoked_other_sessions":1`) {
		t.Fatalf("%d %s", rs.code, rs.body)
	}
	if r.do("GET", "/api/v2/protected", "", nil, a).code != 200 {
		t.Fatal("the device that changed the password stays logged in")
	}
	if r.do("GET", "/api/v2/protected", "", nil, b).code != 401 {
		t.Fatal("other devices must be logged out after a password change")
	}
	if !r.set.VerifyPanelPassword("s3cret-new") {
		t.Fatal("new password not stored")
	}
}

func TestDefaultPasswordMustBeChanged(t *testing.T) {
	clk := &clock{}
	clk.ns.Store(time.Now().UnixNano())
	r := open(t, t.TempDir(), clk) // fresh install: default password "admin"
	r.auth.RequireChange = true
	rs := r.do("POST", "/api/v2/session/login", `{"password":"admin"}`, nil)
	if rs.code != 200 || !strings.Contains(rs.body, `"using_default_password":true`) {
		t.Fatalf("%d %s", rs.code, rs.body)
	}
	c := cookieOf(t, rs)
	var info struct{ CSRF string }
	_ = json.Unmarshal([]byte(rs.body), &info)
	if rs := r.do("GET", "/api/v2/protected", "", nil, c); rs.code != 403 || !strings.Contains(rs.body, "password_change_required") {
		t.Fatalf("panel must stay locked while the default password is in use: %d %s", rs.code, rs.body)
	}
	if rs := r.do("POST", "/api/v2/session/password", `{"current":"admin","new":"admin"}`, map[string]string{CSRFHeader: info.CSRF}, c); rs.code != 400 {
		t.Fatalf("'admin' is not an acceptable new password: %d", rs.code)
	}
	if rs := r.do("POST", "/api/v2/session/password", `{"current":"admin","new":"better-pass"}`, map[string]string{CSRFHeader: info.CSRF}, c); rs.code != 200 {
		t.Fatalf("%d %s", rs.code, rs.body)
	}
	if rs := r.do("GET", "/api/v2/protected", "", nil, c); rs.code != 200 {
		t.Fatalf("after changing it the panel opens: %d", rs.code)
	}
}

func TestDeviceListAndRevoke(t *testing.T) {
	r := newRig(t)
	a, csrfA := r.login(true)
	b, _ := r.login(false)
	rs := r.do("GET", "/api/v2/session/devices", "", nil, a)
	var list struct {
		Devices []struct {
			ID      string `json:"id"`
			Current bool   `json:"current"`
		} `json:"devices"`
	}
	if err := json.Unmarshal([]byte(rs.body), &list); err != nil || len(list.Devices) != 2 {
		t.Fatalf("%d %s", rs.code, rs.body)
	}
	var other string
	for _, d := range list.Devices {
		if !d.Current {
			other = d.ID
		}
	}
	if rs := r.do("DELETE", "/api/v2/session/devices/"+other, "", map[string]string{CSRFHeader: csrfA}, a); rs.code != 200 {
		t.Fatalf("%d %s", rs.code, rs.body)
	}
	if r.do("GET", "/api/v2/protected", "", nil, b).code != 401 {
		t.Fatal("revoked device still works")
	}
	if rs := r.do("DELETE", "/api/v2/session/devices/"+other, "", map[string]string{CSRFHeader: csrfA}, a); rs.code != 404 {
		t.Fatalf("revoking twice: %d", rs.code)
	}
}

func TestExpiredSessionsArePrunedOnLoad(t *testing.T) {
	r := newRig(t)
	cookie, _ := r.login(false)
	r.auth.Store.Close()
	r.clk.advance(48 * time.Hour)
	r2 := open(t, r.dir, r.clk)
	if r2.do("GET", "/api/v2/protected", "", nil, cookie).code != 401 {
		t.Fatal("expired session must not be revived by a restart")
	}
	if n := len(r2.auth.Store.List()); n != 0 {
		t.Fatalf("expired sessions should be dropped, %d left", n)
	}
}

func TestClientKeyParsing(t *testing.T) {
	tp := ParseTrustedProxies("10.0.0.0/8, fd00::/8, 192.168.1.5, junk, ,")
	if len(tp) != 3 {
		t.Fatalf("junk entries must be dropped: %v", tp)
	}
	cases := []struct {
		peer, real, xff, want string
	}{
		{"127.0.0.1", "", "", "127.0.0.1"},
		{"127.0.0.1", "198.51.100.7", "", "198.51.100.7"},
		{"127.0.0.1", "", "198.51.100.8, 10.0.0.1", "198.51.100.8"},
		{"127.0.0.1", "198.51.100.9:443", "", "198.51.100.9"},
		{"127.0.0.1", "[2001:db8::1]:80", "", "2001:db8::1"},
		{"127.0.0.1", "garbage", "198.51.100.10", "198.51.100.10"},
		{"127.0.0.1", "1.1.1.1,2.2.2.2", "", "127.0.0.1"},
		{"::ffff:127.0.0.1", "198.51.100.11", "", "198.51.100.11"},
		{"203.0.113.1", "198.51.100.12", "198.51.100.13", "203.0.113.1"},
		{"192.168.1.5", "198.51.100.14", "", "198.51.100.14"},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "[" + c.peer + "]:1000"
		if !strings.Contains(c.peer, ":") {
			req.RemoteAddr = c.peer + ":1000"
		}
		if c.real != "" {
			req.Header.Set("X-Real-IP", c.real)
		}
		if c.xff != "" {
			req.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := tp.ClientKey(req); got != c.want {
			t.Errorf("peer=%s real=%q xff=%q: got %s want %s", c.peer, c.real, c.xff, got, c.want)
		}
	}
}
