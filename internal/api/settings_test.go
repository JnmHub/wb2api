package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wb2api/internal/activity"
	"wb2api/internal/logbuf"
	"wb2api/internal/ops"
	"wb2api/internal/pool"
	"wb2api/internal/realmswitch"
	"wb2api/internal/scheduler"
	"wb2api/internal/store/accounts"
	"wb2api/internal/store/settings"
	"wb2api/internal/upstream"
)

type sfix struct {
	mux  *http.ServeMux
	st   *settings.Store
	pool *pool.Pool
	ring *logbuf.Ring
	sw   *realmswitch.Switch
	dir  string
}

func newSettingsFixture(t *testing.T) *sfix {
	t.Helper()
	dir := t.TempDir()
	f := &sfix{dir: dir, st: settings.Open(dir), ring: logbuf.New(100), sw: realmswitch.Open(dir, "intl")}
	f.pool = pool.New([]*accounts.Record{accounts.FromMap(map[string]any{"uid": "u1", "accessToken": "t", "realm": "intl",
		"expiresAt": time.Now().Add(time.Hour).Unix()}, "", time.Now())})
	f.mux = http.NewServeMux()
	wrap := func(h http.Handler) http.Handler { return h }
	(&Settings{Store: f.st, Pool: f.pool, Realm: f.sw}).Register(f.mux, wrap)
	(&Logs{Ring: f.ring}).Register(f.mux, wrap)
	client := &ops.Client{Pool: f.pool, Up: upstream.NewManager(upstream.DefaultTimeouts()), Settings: f.st, Activity: &activity.Log{Dir: dir}}
	sched := &scheduler.Scheduler{Pool: f.pool, Ops: client, Settings: f.st, Stagger: func() time.Duration { return 0 }}
	(&Operations{Ops: client, Pool: f.pool, Scheduler: sched, Activity: client.Activity}).Register(f.mux, wrap)
	return f
}

func (f *sfix) do(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestSettingsSnapshotAndPatch(t *testing.T) {
	f := newSettingsFixture(t)
	code, snap := f.do(t, "GET", "/api/v2/settings", "")
	if code != 200 || snap["realm"].(map[string]any)["current"] != "intl" || snap["auth_required"] != false {
		t.Fatalf("%d %v", code, snap)
	}
	if lim := snap["limits"].(map[string]any)["expiring_window_days"].(map[string]any); lim["global"].(float64) != 7 {
		t.Fatalf("%v", lim)
	}
	code, out := f.do(t, "PATCH", "/api/v2/settings", `{
	  "realm":"cn",
	  "switches":{"egress_backoff_enabled":true,"auto_switch_product":true},
	  "auto_switch_products":["cli","vscode"],
	  "upstream":{"header_timeout_seconds":45,"device_token":"abc"},
	  "prompt":{"mode":"append"},
	  "credits_refresh_hours":6,
	  "limits":{"daily_token_limit":{"global":5000,"cn":100}}}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if f.sw.Get() != "cn" || !f.st.EgressBackoff() || !f.st.AutoSwitchProduct() || f.st.PromptConfig().Mode != "append" ||
		f.st.CreditsRefreshHours() != 6 || f.st.Limit(settings.LimitDailyToken, "cn") != 100 || f.st.Limit(settings.LimitDailyToken, "intl") != 5000 {
		t.Fatalf("settings were not applied: %v", out)
	}
	up := out["upstream"].(map[string]any)
	if up["header_timeout_seconds"].(float64) != 45 || up["device_token_set"] != true {
		t.Fatalf("a device token is never echoed back: %v", up)
	}
	for _, bad := range []string{
		`{"realm":"mars"}`, `{"switches":{"nope":true}}`, `{"upstream":{"idle_timeout_seconds":0}}`,
		`{"prompt":{"mode":"weird"}}`, `{"auto_switch_products":["x"]}`, `{"limits":{"nope":{"global":1}}}`, `not json`,
	} {
		if code, _ := f.do(t, "PATCH", "/api/v2/settings", bad); code != 400 {
			t.Errorf("%s -> %d", bad, code)
		}
	}
	// clearing a realm override inherits the global value again
	f.do(t, "PATCH", "/api/v2/settings", `{"limits":{"daily_token_limit":{"clear":["cn"]}}}`)
	if f.st.Limit(settings.LimitDailyToken, "cn") != 5000 {
		t.Error("override not cleared")
	}
}

func TestAPIKeyEndpoints(t *testing.T) {
	f := newSettingsFixture(t)
	code, created := f.do(t, "POST", "/api/v2/keys", `{"name":"ci","realm":"intl","models":["glm-*"],"concurrency_limit":2}`)
	if code != 201 {
		t.Fatalf("%d %v", code, created)
	}
	secret, _ := created["key"].(string)
	if !strings.HasPrefix(secret, "wb-") || created["key_preview"] == secret {
		t.Fatalf("the secret is shown once, masked everywhere else: %v", created)
	}
	_, snap := f.do(t, "GET", "/api/v2/settings", "")
	keys := snap["keys"].([]any)
	if len(keys) != 1 || keys[0].(map[string]any)["key"] != nil || snap["auth_required"] != true {
		t.Fatalf("the list never carries a secret: %v", keys)
	}
	id := created["id"].(string)
	if code, rev := f.do(t, "GET", "/api/v2/keys/"+id+"/secret", ""); code != 200 || rev["key"] != secret {
		t.Fatalf("%d %v", code, rev)
	}
	if code, out := f.do(t, "PATCH", "/api/v2/keys/"+id, `{"enabled":false,"name":"renamed"}`); code != 200 || out["enabled"] != false || out["name"] != "renamed" || out["realm"] != "intl" {
		t.Fatalf("a partial update keeps the rest: %d %v", code, out)
	}
	if code, _ := f.do(t, "POST", "/api/v2/keys", `{"key":"ab"}`); code != 400 {
		t.Errorf("short key: %d", code)
	}
	if code, _ := f.do(t, "DELETE", "/api/v2/keys/"+id, ""); code != 200 {
		t.Fatal("delete")
	}
	if code, _ := f.do(t, "GET", "/api/v2/keys/"+id+"/secret", ""); code != 404 {
		t.Error("a deleted secret is gone")
	}
	_, snap = f.do(t, "GET", "/api/v2/settings", "")
	if k := snap["keys"].([]any)[0].(map[string]any); k["deleted"] != true {
		t.Errorf("history stays visible: %v", k)
	}
}

func TestProxySlotsAreAppliedToTheirAccounts(t *testing.T) {
	f := newSettingsFixture(t)
	if code, out := f.do(t, "PUT", "/api/v2/proxy-slots", `{"slots":[{"url":"http://p1:8080","name":"exit A","enabled":true}]}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	acc := f.pool.Get("u1")
	f.pool.Update("u1", func(r *accounts.Record) { r.ProxySlot = "slot-1" })
	if acc.Proxy() != "http://p1:8080" {
		t.Fatalf("an account bound to a slot goes out through it: %q", acc.Proxy())
	}
	f.do(t, "PUT", "/api/v2/proxy-slots", `{"slots":[{"id":"slot-1","url":"http://p1:8080","enabled":false}]}`)
	if acc.Proxy() != "" {
		t.Errorf("a disabled slot falls back to the account's own proxy: %q", acc.Proxy())
	}
	if code, _ := f.do(t, "PUT", "/api/v2/proxy-slots", `{"slots":[{"url":"ftp://x"}]}`); code != 400 {
		t.Error("bad scheme")
	}
}

func TestLogsEndpoints(t *testing.T) {
	f := newSettingsFixture(t)
	f.ring.Add("INFO", "", "account u1 refreshed", time.Now())
	f.ring.Add("ERROR", "", "upstream exploded", time.Now())
	code, out := f.do(t, "GET", "/api/v2/logs", "")
	if code != 200 || len(out["entries"].([]any)) != 2 || out["last"].(float64) != 2 {
		t.Fatalf("%d %v", code, out)
	}
	_, errs := f.do(t, "GET", "/api/v2/logs?level=ERROR", "")
	if len(errs["entries"].([]any)) != 1 {
		t.Fatalf("%v", errs)
	}
	_, tail := f.do(t, "GET", "/api/v2/logs?after=1", "")
	if len(tail["entries"].([]any)) != 1 {
		t.Fatalf("%v", tail)
	}
	req := httptest.NewRequest("GET", "/api/v2/logs/export", nil)
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "[ERROR]") || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Errorf("%q", w.Body.String())
	}
	f.do(t, "DELETE", "/api/v2/logs", "")
	if _, out := f.do(t, "GET", "/api/v2/logs", ""); len(out["entries"].([]any)) != 0 {
		t.Error("clear")
	}
}

func TestImportExportEndpoints(t *testing.T) {
	f := newSettingsFixture(t)
	tok := "eyJhbGciOiJub25lIn0.eyJpc3MiOiJodHRwczovL3d3dy53b3JrYnVkZHkuYWkiLCJzdWIiOiJuZXctMSIsImV4cCI6OTk5OTk5OTk5OX0.sig"
	doc := `{"accounts":[{"accessToken":"` + tok + `","nickname":"imp"}]}`
	body, _ := json.Marshal(map[string]any{"document": doc, "dry_run": true})
	code, out := f.do(t, "POST", "/api/v2/accounts/import", string(body))
	if code != 200 || out["dry_run"] != true || len(out["result"].(map[string]any)["added"].([]any)) != 1 || f.pool.Len() != 1 {
		t.Fatalf("%d %v", code, out)
	}
	body, _ = json.Marshal(map[string]any{"document": doc})
	if code, _ := f.do(t, "POST", "/api/v2/accounts/import", string(body)); code != 200 || f.pool.Len() != 2 {
		t.Fatalf("import: %d len=%d", code, f.pool.Len())
	}
	// an inline object works as well
	body, _ = json.Marshal(map[string]any{"document": map[string]any{"foo": 1}})
	if code, _ := f.do(t, "POST", "/api/v2/accounts/import", string(body)); code != 400 {
		t.Error("not an account document")
	}
	if code, _ := f.do(t, "POST", "/api/v2/accounts/import", `{}`); code != 400 {
		t.Error("document required")
	}
	req := httptest.NewRequest("GET", "/api/v2/accounts/export?uids=u1&secrets=0", nil)
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, req)
	if w.Code != 200 || strings.Contains(w.Body.String(), "accessToken") || !strings.Contains(w.Body.String(), `"count": 1`) {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
}

func TestSchedulerEndpoints(t *testing.T) {
	f := newSettingsFixture(t)
	code, st := f.do(t, "GET", "/api/v2/scheduler", "")
	if code != 200 || st["enabled"] != true || st["next_run_time"] == "待调度" {
		t.Fatalf("%d %v", code, st)
	}
	code, st = f.do(t, "PATCH", "/api/v2/scheduler", `{"enabled":false,"schedule":{"checkin":[7]}}`)
	if code != 200 || st["enabled"] != false || st["schedule"].(map[string]any)["checkin"].([]any)[0].(float64) != 7 {
		t.Fatalf("%d %v", code, st)
	}
	if code, _ := f.do(t, "PATCH", "/api/v2/scheduler", `{"schedule":{"checkin":[]}}`); code != 400 {
		t.Error("empty hours are refused")
	}
	if code, _ := f.do(t, "POST", "/api/v2/scheduler/trigger", `{}`); code != 202 && code != 409 {
		t.Errorf("trigger: %d", code)
	}
}

func TestActivityEndpointValidatesTheFilter(t *testing.T) {
	f := newSettingsFixture(t)
	if code, out := f.do(t, "GET", "/api/v2/activity?range=all", ""); code != 200 || out["total"].(float64) != 0 {
		t.Fatalf("%d %v", code, out)
	}
	if code, _ := f.do(t, "GET", "/api/v2/activity?result=maybe", ""); code != 400 {
		t.Error("a bad filter is a 400, not silently ignored")
	}
}

func TestActionOnUnknownAccount(t *testing.T) {
	f := newSettingsFixture(t)
	if code, _ := f.do(t, "POST", "/api/v2/accounts/ghost/actions/credits", `{}`); code != 404 {
		t.Error("unknown account")
	}
	if code, _ := f.do(t, "POST", "/api/v2/accounts/u1/actions/nonsense", `{}`); code != 404 {
		t.Error("unknown action")
	}
}
