package openai

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/catalog"
	"wb2api/internal/pool"
	"wb2api/internal/store/accounts"
	"wb2api/internal/store/settings"
	"wb2api/internal/upstream"
)

type infoEnv struct {
	srv     *httptest.Server
	configs atomic.Int32
	cat     *catalog.Service
	p       *pool.Pool
	mux     *http.ServeMux
}

const remoteConfig = `{"code":0,"data":{
 "agents":[{"name":"cli","models":["hy3","glm-5.3","default-model","glm-5.3-sg","gpt-5.5","brand-new","creditless"]}],
 "models":[
  {"id":"hy3","name":"HY3 live","credits":"x0.00","supportsToolCall":true},
  {"id":"glm-5.3","name":"GLM 5.3","credits":"x1.00"},
  {"id":"gpt-5.5","credits":"x2.00"},
  {"id":"brand-new","name":"Brand New","credits":"x3.00","supportsImages":true,
   "reasoning":{"supportedEfforts":["low","high"],"effort":"high"}},
  {"id":"creditless","name":"No rate"}
 ]}}`

func newInfoEnv(t *testing.T, settingsJSON string, creds map[string]map[string]any, cfgStatus int) *infoEnv {
	t.Helper()
	e := &infoEnv{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/config" {
			http.NotFound(w, r)
			return
		}
		e.configs.Add(1)
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer token-") || r.Header.Get("X-User-Id") == "" {
			t.Errorf("config call lacks credentials: %v", r.Header)
		}
		if cfgStatus != 200 {
			http.Error(w, "nope", cfgStatus)
			return
		}
		io.WriteString(w, remoteConfig)
	}))
	t.Cleanup(up.Close)

	dir := t.TempDir()
	exp := time.Now().Add(24 * time.Hour).Unix()
	var recs []*accounts.Record
	for i := 0; i < 3; i++ {
		m := map[string]any{
			"uid": fmt.Sprintf("acct-%d", i), "accessToken": fmt.Sprintf("token-%d", i),
			"realm": "intl", "expiresAt": exp,
		}
		if i == 2 {
			m["realm"] = "cn"
		}
		if c, ok := creds[fmt.Sprint(i)]; ok {
			m["credits"] = c
		}
		recs = append(recs, accounts.FromMap(m, "", time.Now()))
	}
	if settingsJSON != "" {
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settingsJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := pool.New(recs)
	h := &Handler{Pool: p, Up: upstream.NewManager(upstream.DefaultTimeouts()), Settings: settings.Open(dir)}
	e.cat = &catalog.Service{
		Pool: p, Up: h.Up, Dir: dir, CacheDir: filepath.Join(dir, "catalog"),
		UpstreamOverride: up.URL, Home: t.TempDir(),
	}
	h.Catalog = e.cat
	mux := http.NewServeMux()
	(&Info{Chat: h, Pool: p, Catalog: e.cat}).Register(mux)
	e.p, e.mux = p, mux
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *infoEnv) get(t *testing.T, path string, hdr map[string]string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("%s: not JSON: %q", path, body)
	}
	return resp.StatusCode, out
}

func ids(out map[string]any) []string {
	var got []string
	for _, m := range out["data"].([]any) {
		got = append(got, m.(map[string]any)["id"].(string))
	}
	return got
}

func TestModelsLiveCatalogue(t *testing.T) {
	e := newInfoEnv(t, "", nil, 200)
	code, out := e.get(t, "/v1/models", nil)
	if code != 200 || out["object"] != "list" || out["realm"] != "intl" {
		t.Fatalf("%d %v", code, out)
	}
	got := strings.Join(ids(out), ",")
	// the bundled picker order is kept (names upstream no longer lists stay);
	// aliases, -sg variants and rate-less models are not advertised; the new
	// model is appended after the curated order
	if !strings.HasPrefix(got, "hy4-preview-f,hy3,") || !strings.HasSuffix(got, ",brand-new") {
		t.Fatalf("models = %s", got)
	}
	for _, bad := range []string{"default-model", "glm-5.3-sg", "creditless", "auto"} {
		if strings.Contains(got, bad) {
			t.Errorf("%s must not be advertised: %s", bad, got)
		}
	}
	cat := out["catalog"].(map[string]any)
	if cat["source"] != "remote" || cat["fetched_at"].(float64) < 1e9 || cat["file_mtime"] != nil {
		t.Errorf("catalog meta %v", cat)
	}
	var hy3, nw map[string]any
	for _, m := range out["data"].([]any) {
		switch m.(map[string]any)["id"] {
		case "hy3":
			hy3 = m.(map[string]any)
		case "brand-new":
			nw = m.(map[string]any)
		}
	}
	if hy3["name"] != "HY3 live" || hy3["supports_tool_calls"] != true || hy3["owned_by"] != "workbuddy" {
		t.Errorf("live metadata must override the bundled entry: %v", hy3)
	}
	if nw["supports_vision"] != true || nw["context_length"].(float64) != 1_000_000 || nw["reasoning_default_effort"] != "high" {
		t.Errorf("new model entry: %v", nw)
	}
	if _, has := nw["reasoning_fixed_effort"]; has {
		t.Error("an effort next to supportedEfforts is a default, not a pin")
	}
	// archived copy of the answer
	if _, err := os.Stat(filepath.Join(e.cat.CacheDir, "product-config-intl.json")); err != nil {
		t.Errorf("config archive: %v", err)
	}
}

func TestModelsCachedFiveMinutes(t *testing.T) {
	e := newInfoEnv(t, "", nil, 200)
	for i := 0; i < 5; i++ {
		if code, _ := e.get(t, "/v1/models", nil); code != 200 {
			t.Fatal(code)
		}
	}
	if n := e.configs.Load(); n != 1 {
		t.Fatalf("upstream /v3/config hit %d times for 5 listings, want 1", n)
	}
	// the other path serves the same cache
	if code, _ := e.get(t, "/models", nil); code != 200 || e.configs.Load() != 1 {
		t.Fatal("bare /models must share the cache")
	}
}

func TestModelsFallBackToBundledSnapshot(t *testing.T) {
	e := newInfoEnv(t, "", nil, 500)
	code, out := e.get(t, "/v1/models", nil)
	if code != 200 {
		t.Fatalf("a dead product-config endpoint must not break /v1/models: %d", code)
	}
	if src := out["catalog"].(map[string]any)["source"]; src != "bundled" {
		t.Errorf("source = %v", src)
	}
	got := ids(out)
	if len(got) < 5 || got[0] != "hy4-preview-f" {
		t.Errorf("bundled order: %v", got)
	}
}

func TestModelsRealmSelection(t *testing.T) {
	e := newInfoEnv(t, "", nil, 200)
	_, cn := e.get(t, "/v1/models?realm=cn", nil)
	if cn["realm"] != "cn" {
		t.Fatalf("%v", cn["realm"])
	}
	if !strings.Contains(strings.Join(ids(cn), ","), "deepseek-v4-pro") {
		t.Errorf("cn list: %v", ids(cn))
	}
	_, hdr := e.get(t, "/v1/models", map[string]string{"X-Realm": "cn"})
	if hdr["realm"] != "cn" {
		t.Errorf("X-Realm ignored: %v", hdr["realm"])
	}
	_, junk := e.get(t, "/v1/models?realm=../../etc", nil)
	if junk["realm"] != "intl" {
		t.Errorf("unknown realm must fall back to the default: %v", junk["realm"])
	}
}

func TestModelsKeyAuthAndBoundRealm(t *testing.T) {
	e := newInfoEnv(t, `{"api_keys":[{"id":"k1","name":"cn key","key":"sk-cn","enabled":true,"realm":"cn"}]}`, nil, 200)
	if code, _ := e.get(t, "/v1/models", nil); code != 401 {
		t.Fatalf("no key: %d", code)
	}
	if code, _ := e.get(t, "/v1/models", map[string]string{"Authorization": "Bearer wrong"}); code != 401 {
		t.Fatalf("wrong key: %d", code)
	}
	code, out := e.get(t, "/v1/models", map[string]string{"Authorization": "Bearer sk-cn", "X-Realm": "intl"})
	if code != 200 || out["realm"] != "cn" {
		t.Fatalf("a key bound to cn wins over X-Realm: %d %v", code, out["realm"])
	}
}

func TestBannedModelIsHiddenFromTheCatalogue(t *testing.T) {
	e := newInfoEnv(t, `{"banned_models":["hy3"]}`, nil, 200)
	code, out := e.get(t, "/v1/models", nil)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	list := ids(out)
	if len(list) == 0 {
		t.Fatal("no models returned")
	}
	for _, id := range list {
		if id == "hy3" {
			t.Fatalf("a banned model must not be listed: %v", list)
		}
	}
}

func TestBannedModelIsRefusedBeforeTheUpstream(t *testing.T) {
	up := newRawUpstream(t, func(n int, r rawReq, c net.Conn) { sseOK(c) })
	f := newFixture(t, up, accSpec{})
	f.setSettings(t, `{"banned_models":["hy3"],"banned_substring":"image-alpha"}`)
	code, body, _ := f.post(t, `{"model":"hy3","messages":[{"role":"user","content":"x"}]}`, nil)
	if code != 400 || !strings.Contains(body, "封锁") {
		t.Fatalf("banned model: %d %s", code, body)
	}
	if n := len(up.requests()); n != 0 {
		t.Fatalf("a banned model must not reach the upstream (%d requests)", n)
	}
	code, body, _ = f.post(t, `{"model":"flux-image-alpha-v2","messages":[{"role":"user","content":"x"}]}`, nil)
	if code != 400 || !strings.Contains(body, "封锁") {
		t.Fatalf("banned substring: %d %s", code, body)
	}
	code, _, _ = f.post(t, `{"model":"glm-5.3","messages":[{"role":"user","content":"x"}]}`, nil)
	if code != 200 {
		t.Fatalf("an allowed model must still pass: %d", code)
	}
}

func TestPinnedRouteListsTheAccountCatalogue(t *testing.T) {
	e := newInfoEnv(t, "", nil, 200)
	srv := httptest.NewServer(PinnedRoutes(e.p, e.mux))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/u/acct-0/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out map[string]any
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("not JSON: %q", body)
	}
	if out["realm"] != "intl" {
		t.Fatalf("a pinned route uses the account's realm: %v", out["realm"])
	}
	if len(ids(out)) == 0 {
		t.Fatal("no models for the pinned account")
	}
}

func TestBalanceSummary(t *testing.T) {
	e := newInfoEnv(t, "", map[string]map[string]any{
		"0": {"remain": 10.25, "updated_at": 1790000100.5},
		"1": {"remain": "4.5", "updated_at": 1790000200},
		"2": {"remain": 100.0, "updated_at": 1790000300},
	}, 200)
	_, out := e.get(t, "/v1/balance-summary", nil)
	if out["balance"].(float64) != 14.75 || out["accounts_enabled"].(float64) != 2 ||
		out["accounts_with_credit_data"].(float64) != 2 || out["unit"] != "credit" ||
		out["updated_at"].(float64) != 1790000200 || out["realm"] != "intl" {
		t.Errorf("intl: %v", out)
	}
	for k := range out {
		if k == "uid" || k == "accounts" {
			t.Errorf("must not identify accounts: %v", out)
		}
	}
	_, all := e.get(t, "/v1/balance-summary?realm=all", nil)
	if all["balance"].(float64) != 114.75 || all["accounts_enabled"].(float64) != 3 {
		t.Errorf("all: %v", all)
	}
}

func TestBalanceSummaryUnknownStaysUnknown(t *testing.T) {
	e := newInfoEnv(t, "", map[string]map[string]any{"0": {"remain": -5}, "1": {"total": 9}}, 200)
	_, out := e.get(t, "/v1/balance-summary", nil)
	if out["balance"] != nil || out["updated_at"] != nil || out["accounts_with_credit_data"].(float64) != 0 || out["accounts_enabled"].(float64) != 2 {
		t.Errorf("unknown balance must be null, not zero: %v", out)
	}
}

func TestHealth(t *testing.T) {
	e := newInfoEnv(t, "", nil, 200)
	code, out := e.get(t, "/health", nil)
	if code != 200 || out["ok"] != true || out["accounts"].(float64) != 3 || out["accounts_ready"].(float64) != 3 ||
		out["api_key_required"] != false || out["realm"] != "intl" || out["uid"] != "acct-0" {
		t.Fatalf("%d %v", code, out)
	}

	k := newInfoEnv(t, `{"api_keys":[{"id":"k1","key":"sk-1","enabled":true}]}`, nil, 200)
	code, anon := k.get(t, "/health", nil)
	if code != 200 || anon["ok"] != true || anon["api_key_required"] != true {
		t.Fatalf("health must answer without a key: %d %v", code, anon)
	}
	if _, has := anon["uid"]; has {
		t.Errorf("identity leaked to an anonymous caller: %v", anon)
	}
	_, authed := k.get(t, "/health", map[string]string{"Authorization": "Bearer sk-1"})
	if authed["uid"] != "acct-0" {
		t.Errorf("authorised health shows identity: %v", authed)
	}
}

func TestInfoRefusesWrites(t *testing.T) {
	e := newInfoEnv(t, "", nil, 200)
	for _, p := range []string{"/v1/models", "/v1/balance-summary", "/health"} {
		resp, err := http.Post(e.srv.URL+p, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 405 {
			t.Errorf("POST %s -> %d", p, resp.StatusCode)
		}
	}
}

func TestManualRefreshCooldown(t *testing.T) {
	e := newInfoEnv(t, "", nil, 200)
	first := e.cat.Refresh(t.Context(), "intl")
	if !first.OK || first.Count == 0 || first.Source != "remote" {
		t.Fatalf("%+v", first)
	}
	again := e.cat.Refresh(t.Context(), "intl")
	if again.OK || !again.Cooldown || again.RetryAfter <= 0 {
		t.Fatalf("a second manual refresh inside 30s is refused: %+v", again)
	}
	if n := e.configs.Load(); n != 1 {
		t.Fatalf("refused refresh must not touch upstream: %d calls", n)
	}
	if bad := e.cat.Refresh(t.Context(), "mars"); bad.OK {
		t.Fatal("unknown realm")
	}
}

func TestFailedRefreshKeepsPreviousList(t *testing.T) {
	e := newInfoEnv(t, "", nil, 200)
	_, before := e.get(t, "/v1/models", nil)
	e.cat.UpstreamOverride = "http://127.0.0.1:1" // nothing listens
	res := e.cat.Refresh(t.Context(), "intl")
	if res.OK {
		t.Fatalf("%+v", res)
	}
	_, after := e.get(t, "/v1/models", nil)
	if strings.Join(ids(before), ",") != strings.Join(ids(after), ",") {
		t.Error("a failed refresh must keep the previous list")
	}
}

func TestModelsFilteredByKeyWhenEnabled(t *testing.T) {
	settingsJSON := `{"filter_models_by_key":true,"api_keys":[{"id":"k1","name":"limited","key":"sk-limited","enabled":true,"models":["glm-*"]}]}`
	e := newInfoEnv(t, settingsJSON, nil, 200)
	code, out := e.get(t, "/v1/models", map[string]string{"Authorization": "Bearer sk-limited"})
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	list := ids(out)
	if len(list) == 0 {
		t.Fatal("no models")
	}
	for _, id := range list {
		if !strings.HasPrefix(id, "glm-") {
			t.Fatalf("a key restricted to glm-* sees %q: %v", id, list)
		}
	}

	// The switch is opt-in: off keeps the full catalogue for the same key.
	e2 := newInfoEnv(t, `{"api_keys":[{"id":"k1","name":"limited","key":"sk-limited","enabled":true,"models":["glm-*"]}]}`, nil, 200)
	_, out2 := e2.get(t, "/v1/models", map[string]string{"Authorization": "Bearer sk-limited"})
	full := ids(out2)
	hasHy := false
	for _, id := range full {
		if id == "hy3" {
			hasHy = true
		}
	}
	if !hasHy {
		t.Fatalf("filter off must keep the full catalogue: %v", full)
	}
}

func TestModelAliasesAreAdvertised(t *testing.T) {
	settingsJSON := `{"model_aliases":{"claude-sonnet-4":"glm-5.3"}}`
	e := newInfoEnv(t, settingsJSON, nil, 200)
	code, out := e.get(t, "/v1/models", nil)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	var alias map[string]any
	for _, m := range out["data"].([]any) {
		item := m.(map[string]any)
		if item["id"] == "claude-sonnet-4" {
			alias = item
		}
	}
	if alias == nil {
		t.Fatalf("the alias is missing from the catalogue: %v", ids(out))
	}
	if alias["alias_of"] != "glm-5.3" {
		t.Fatalf("alias_of = %v; want glm-5.3", alias["alias_of"])
	}
	// The real id stays listed next to it.
	found := false
	for _, id := range ids(out) {
		if id == "glm-5.3" {
			found = true
		}
	}
	if !found {
		t.Fatal("the real model must stay in the catalogue next to its alias")
	}
}

func TestModelsOnlyAvailableToggleHidesNothingWhenOff(t *testing.T) {
	e := newInfoEnv(t, `{"isolate":true}`, nil, 200)
	_, out := e.get(t, "/v1/models", nil)
	full := len(ids(out))
	e2 := newInfoEnv(t, `{"models_only_available":false}`, nil, 200)
	_, out2 := e2.get(t, "/v1/models", nil)
	if got := len(ids(out2)); got != full {
		t.Fatalf("filter off returned %d models, want the full catalogue (%d)", got, full)
	}
}
func TestModelAliasResolvesBeforeTheUpstream(t *testing.T) {
	up := newRawUpstream(t, func(n int, r rawReq, c net.Conn) { sseOK(c) })
	f := newFixture(t, up, accSpec{})
	f.setSettings(t, `{"model_aliases":{"claude-sonnet-4":"glm-5.3"}}`)
	code, body, _ := f.post(t, `{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}]}`, nil)
	if code != 200 {
		t.Fatalf("alias request failed: %d %s", code, body)
	}
	reqs := up.requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream calls = %d; want 1", len(reqs))
	}
	sent, err := reqs[0].jsonBody(t).Get("model").(string), error(nil)
	if err != nil {
		t.Fatal(err)
	}
	if sent != "glm-5.3" {
		t.Fatalf("upstream model = %q; want the resolved id glm-5.3", sent)
	}
	// A deny-listed real id also blocks its alias, because the check runs after
	// resolution.
	f.setSettings(t, `{"model_aliases":{"claude-sonnet-4":"glm-5.3"},"banned_models":["glm-5.3"]}`)
	code, body, _ = f.post(t, `{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}]}`, nil)
	if code != 400 {
		t.Fatalf("alias of a banned model must be refused: %d %s", code, body)
	}
}
