package ops

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wb2api/internal/activity"
	"wb2api/internal/pool"
	"wb2api/internal/store/accounts"
	"wb2api/internal/store/settings"
	"wb2api/internal/upstream"
)

func jwt(iss, sub string, exp int64) string {
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	return enc(map[string]any{"alg": "none"}) + "." + enc(map[string]any{"iss": iss, "sub": sub, "exp": exp}) + ".sig"
}

type vendor struct {
	t     *testing.T
	mu    sync.Mutex
	calls []string
	h     map[string]http.HandlerFunc
}

func newVendor(t *testing.T) (*vendor, *httptest.Server) {
	v := &vendor{t: t, h: map[string]http.HandlerFunc{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		v.calls = append(v.calls, r.Method+" "+r.URL.Path)
		h := v.h[r.URL.Path]
		v.mu.Unlock()
		if h == nil {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return v, srv
}

func (v *vendor) json(path, body string) {
	v.h[path] = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}
}

func (v *vendor) called(sub string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	n := 0
	for _, c := range v.calls {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

type env struct {
	c       *Client
	pool    *pool.Pool
	vendor  *vendor
	saved   []*accounts.Record
	savedMu sync.Mutex
	dir     string
}

func newEnv(t *testing.T, realmName string, extra map[string]any) *env {
	t.Helper()
	v, srv := newVendor(t)
	e := &env{vendor: v, dir: t.TempDir()}
	iss := "https://www.workbuddy.ai"
	if realmName == "cn" {
		iss = "https://www.codebuddy.cn"
	}
	m := map[string]any{"uid": "u-1", "accessToken": jwt(iss, "u-1", time.Now().Add(24*time.Hour).Unix()), "refreshToken": "r",
		"realm": realmName, "expiresAt": time.Now().Add(24 * time.Hour).Unix()}
	for k, val := range extra {
		m[k] = val
	}
	e.pool = pool.New([]*accounts.Record{accounts.FromMap(m, "", time.Now())})
	e.c = &Client{
		Pool: e.pool, Up: upstream.NewManager(upstream.DefaultTimeouts()), Settings: settings.Open(e.dir),
		Activity: &activity.Log{Dir: e.dir}, BaseOverride: srv.URL,
		Persist: func(r *accounts.Record) error {
			e.savedMu.Lock()
			e.saved = append(e.saved, r)
			e.savedMu.Unlock()
			return nil
		},
	}
	return e
}

func (e *env) acc() *pool.Account { return e.pool.All()[0] }

func TestCreditsCN(t *testing.T) {
	e := newEnv(t, "cn", nil)
	end := time.Now().Add(3 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	e.vendor.json("/billing/meter/get-user-resource-summary", `{"code":0,"data":{"IsPaidUser":false,"Packages":[{"PackageCode":"free"},{"PackageCode":"bonus"}]}}`)
	e.vendor.json("/billing/meter/get-user-resource-free-packages", fmt.Sprintf(`{"data":{"Accounts":[
	  {"AccountId":"a1","PackageName":"Free","PackageCode":"free","CycleCapacitySizePrecise":"100","CycleCapacityRemainPrecise":"60","CycleEndTime":%q},
	  {"AccountId":"a2","PackageName":"Bonus Pack","PackageCode":"bonus","CapacitySize":50,"CapacityRemain":50,"DeductionEndTime":%d}]}}`,
		"2099-01-01 00:00:00", time.Now().Add(2*24*time.Hour).UnixMilli()))
	e.vendor.json("/billing/meter/get-user-resource-paid-packages", fmt.Sprintf(`{"data":{"Accounts":[
	  {"AccountId":"a2","PackageName":"dup","CapacitySize":999,"CapacityRemain":999},
	  {"AccountId":"a3","PackageName":"Paid","PackageCode":"paid","CycleCapacitySize":10,"CycleCapacityRemain":0,"CycleCapacityUsed":10,"CycleEndTime":%q}]}}`, end))
	e.vendor.json("/v2/billing/meter/checkin-activity-status", `{"data":{"today_checked_in":true,"streak_days":4,"daily_credit":50,"today_credit":50}}`)

	// an earlier 402 park must be lifted by a refresh that shows credits
	e.acc().NoteBalanceCooled("402", time.Now())
	res := e.c.FetchCredits(context.Background(), e.acc())
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	cr := e.acc().Record().Credits
	if cr["remain"].(float64) != 110 || cr["size"].(float64) != 160 || cr["used"].(float64) != 50 {
		t.Fatalf("totals: %v", cr)
	}
	pkgs := cr["packages"].([]any)
	if len(pkgs) != 3 {
		t.Fatalf("the duplicate AccountId must be skipped: %d packages", len(pkgs))
	}
	first := pkgs[0].(map[string]any)
	if first["in_usage"] != true {
		t.Errorf("packages being drawn on come first: %v", first)
	}
	bonus := findPkg(pkgs, "Bonus Pack")
	if d, ok := bonus["days_left"].(float64); !ok || d < 1.5 || d > 2.1 {
		t.Errorf("expiry reads the deduction deadline, not the cycle end: %v", bonus["days_left"])
	}
	if free := findPkg(pkgs, "Free"); free["no_expiry"] != true {
		t.Errorf("a far-future cycle end is no expiry: %v", free)
	}
	if ci := cr["checkin"].(map[string]any); ci["streak_days"].(int64) != 4 || ci["today_checked_in"] != true {
		t.Errorf("checkin %v", ci)
	}
	if ee := cr["earliest_expiring"].(map[string]any); ee["name"] != "Bonus Pack" {
		t.Errorf("earliest expiring %v", ee)
	}
	if !e.acc().Ready("m", time.Now()) {
		t.Error("the refresh lifts the 402 park")
	}
	if len(e.saved) == 0 {
		t.Error("persisted")
	}
	// billing headers carry the account identity
	if e.vendor.called("get-user-resource-summary") != 1 {
		t.Error("one summary call")
	}
}

func findPkg(pkgs []any, name string) map[string]any {
	for _, p := range pkgs {
		if m := p.(map[string]any); m["name"] == name {
			return m
		}
	}
	return nil
}

func TestCreditsIntlFallbackAndEnterprise(t *testing.T) {
	e := newEnv(t, "intl", nil)
	e.vendor.json("/v2/billing/meter/get-user-resource", `{"data":{"Response":{"Data":{"Accounts":[
	  {"PackageName":"Free","CycleCapacitySize":30,"CycleCapacityRemain":12.345}]}}}}`)
	if res := e.c.FetchCredits(context.Background(), e.acc()); !res.OK {
		t.Fatalf("%+v", res)
	}
	if cr := e.acc().Record().Credits; cr["remain"].(float64) != 12.35 && cr["remain"].(float64) != 12.34 || cr["size"].(float64) != 30 {
		t.Fatalf("%v", cr)
	}

	ent := newEnv(t, "cn", map[string]any{"enterpriseId": "ent-1"})
	ent.vendor.json("/billing/meter/get-user-resource-summary", `{"code":0,"data":{"Packages":[]}}`)
	ent.vendor.json("/billing/meter/get-enterprise-user-usage", `{"data":{"credit":"30.5","limitNum":100,"cycleStartTime":"2026-10-01 00:00:00","cycleEndTime":"2026-11-01 00:00:00"}}`)
	if res := ent.c.FetchCredits(context.Background(), ent.acc()); !res.OK {
		t.Fatalf("%+v", res)
	}
	cr := ent.acc().Record().Credits
	if cr["remain"].(float64) != 69.5 || cr["is_enterprise"] != true || cr["earliest_expiring"] != nil {
		t.Fatalf("an enterprise quota resets, it never expires: %v", cr)
	}
}

func TestCredits401MarksReloginNeeded(t *testing.T) {
	e := newEnv(t, "intl", nil)
	e.vendor.h["/v2/billing/meter/get-user-resource"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"msg":"Unauthorized"}`)
	}
	res := e.c.FetchCredits(context.Background(), e.acc())
	if res.OK || !res.Relogin || !e.acc().TokenBad() {
		t.Fatalf("%+v tokenBad=%v", res, e.acc().TokenBad())
	}
}

func TestCheckin(t *testing.T) {
	e := newEnv(t, "cn", nil)
	e.vendor.json("/v2/billing/meter/daily-checkin", `{"code":0,"msg":"签到成功","data":{"credit":50}}`)
	if !e.c.CanCheckin(e.acc()) {
		t.Fatal("never checked in")
	}
	res := e.c.Checkin(context.Background(), e.acc(), "manual")
	if !res.OK || e.c.CanCheckin(e.acc()) {
		t.Fatalf("%+v can=%v", res, e.c.CanCheckin(e.acc()))
	}
	hist := e.c.Activity.Query(activity.Filter{})
	if hist.Total != 1 || hist.Rows[0].Task != "checkin" || hist.Rows[0].Trigger != "manual" || !hist.Rows[0].OK {
		t.Fatalf("history %+v", hist)
	}

	// "already checked in today" is an HTTP 400 with code 10001: that counts as done
	e2 := newEnv(t, "cn", nil)
	e2.vendor.h["/v2/billing/meter/daily-checkin"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"code":10001,"msg":"今天已签到"}`)
	}
	if res := e2.c.Checkin(context.Background(), e2.acc(), "scheduler"); !res.OK || e2.c.CanCheckin(e2.acc()) {
		t.Fatalf("%+v", res)
	}

	intl := newEnv(t, "intl", nil)
	if res := intl.c.Checkin(context.Background(), intl.acc(), "manual"); res.OK || !strings.Contains(res.Error, "only available for CN") {
		t.Fatalf("%+v", res)
	}
	enterprise := newEnv(t, "cn", map[string]any{"enterpriseId": "e"})
	if enterprise.c.CanCheckin(enterprise.acc()) {
		t.Fatal("enterprise accounts do not check in")
	}
}

func TestReportActivityReadsTheStreak(t *testing.T) {
	e := newEnv(t, "cn", nil)
	var body string
	e.vendor.h["/v2/report"] = func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		io.WriteString(w, `{"code":0}`)
	}
	e.vendor.json("/activity/growth/streak", `{"data":{"streak":{"days":7}}}`)
	res := e.c.ReportActivity(context.Background(), e.acc(), "scheduler")
	if !res.OK || !strings.Contains(res.Msg, "7 天") || e.c.CanReportActivity(e.acc()) {
		t.Fatalf("%+v", res)
	}
	var events []map[string]any
	if err := json.Unmarshal([]byte(body), &events); err != nil || len(events) != 1 || events[0]["eventCode"] != "chat_request_send" || events[0]["userId"] != "u-1" {
		t.Fatalf("the event must carry userId or the upstream drops it: %s", body)
	}
	// the upstream refusing the report is a failure, and nothing is marked done
	e2 := newEnv(t, "cn", nil)
	e2.vendor.json("/v2/report", `{"code":1}`)
	if res := e2.c.ReportActivity(context.Background(), e2.acc(), "manual"); res.OK || !e2.c.CanReportActivity(e2.acc()) {
		t.Fatalf("%+v", res)
	}
}

func TestDailyChat(t *testing.T) {
	e := newEnv(t, "intl", nil)
	var sent map[string]any
	e.vendor.h["/v2/chat/completions"] = func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &sent)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	}
	e.vendor.json("/v2/billing/meter/get-user-resource", `{"data":{"Response":{"Data":{"Accounts":[]}}}}`)
	res := e.c.DailyChat(context.Background(), e.acc(), "scheduler")
	if !res.OK || e.c.CanDailyChat(e.acc()) {
		t.Fatalf("%+v", res)
	}
	if sent["model"] != "deepseek-v4.1-flash" || sent["stream"] != true || sent["max_tokens"].(float64) != 10 {
		t.Fatalf("the activity is a throwaway free-model conversation: %v", sent)
	}
	// the day's activity is skipped when the model is cooling: it moves to another free one
	e.acc().NoteModelError("deepseek-v4.1-flash", time.Time{}, time.Hour, "429", "", time.Now())
	if m := e.c.dailyModel(e.acc()); m != "hy3" {
		t.Errorf("fallback model %q", m)
	}
}

func TestOAuthLogin(t *testing.T) {
	e := newEnv(t, "intl", nil)
	e.vendor.json("/v2/plugin/auth/state", `{"data":{"state":"st-1","authUrl":"https://login.example/auth?x=1"}}`)
	polls := 0
	newTok := jwt("https://www.workbuddy.ai", "new-user", time.Now().Add(24*time.Hour).Unix())
	e.vendor.h["/v2/plugin/auth/token"] = func(w http.ResponseWriter, r *http.Request) {
		polls++
		if polls == 1 {
			io.WriteString(w, `{"code":11217,"msg":"waiting"}`)
			return
		}
		fmt.Fprintf(w, `{"code":0,"data":{"accessToken":%q,"refreshToken":"rt","domain":"www.workbuddy.ai"}}`, newTok)
	}
	e.vendor.json("/v2/plugin/login/account", `{"data":{"nickname":"Newbie"}}`)
	start, err := e.c.LoginStart(context.Background(), "intl", "CLI")
	if err != nil || start["state"] != "st-1" || !strings.HasPrefix(start["authUrl"].(string), "https://login.example") {
		t.Fatalf("%v %v", start, err)
	}
	if p := e.c.LoginPoll(context.Background(), "st-1"); p["status"] != "pending" {
		t.Fatalf("%v", p)
	}
	p := e.c.LoginPoll(context.Background(), "st-1")
	if p["status"] != "ok" || e.pool.Get("new-user") == nil || e.pool.Get("new-user").Record().Nickname != "Newbie" {
		t.Fatalf("%v", p)
	}
	if p := e.c.LoginPoll(context.Background(), "st-1"); p["status"] != "unknown" {
		t.Errorf("a finished login is forgotten: %v", p)
	}
	if p := e.c.LoginPoll(context.Background(), "nope"); p["status"] != "unknown" {
		t.Errorf("%v", p)
	}
}

func TestImportExport(t *testing.T) {
	e := newEnv(t, "intl", nil)
	cnTok := jwt("https://www.codebuddy.cn", "cn-user", time.Now().Add(time.Hour).Unix())
	intlTok := jwt("https://www.workbuddy.ai", "intl-user", time.Now().Add(time.Hour).Unix())
	doc := fmt.Sprintf(`{"accounts":[
	  {"accessToken":%q,"nickname":"cn","realm":"intl"},
	  {"accessToken":%q,"uid":"weird uid/!"},
	  {"accessToken":"not-a-jwt"},
	  {"nickname":"no token"},
	  {"access_token":%q,"email":"x@y.z","expires_at":%d},
	  {"auth":{"accessToken":%q,"expiresAt":%d},"account":{"uid":"u-1","nickname":"existing"}}]}`,
		cnTok, intlTok, jwt("https://www.workbuddy.ai", "snake", time.Now().Add(time.Hour).Unix()), time.Now().Add(time.Hour).UnixMilli(),
		jwt("https://www.workbuddy.ai", "u-1", time.Now().Add(time.Hour).Unix()), time.Now().Add(time.Hour).UnixMilli())
	rows, err := ParseDocument([]byte(doc))
	if err != nil || len(rows) != 6 {
		t.Fatalf("%d %v", len(rows), err)
	}
	dry := e.c.Import(rows, "", false, true)
	if len(dry.Added) != 3 || len(dry.Skipped) != 1 || len(dry.Invalid) != 2 || e.pool.Len() != 1 {
		t.Fatalf("a dry run reports without touching the pool: %+v len=%d", dry, e.pool.Len())
	}
	real := e.c.Import(rows, "", false, false)
	if len(real.Added) != 3 || e.pool.Len() != 4 {
		t.Fatalf("%+v", real)
	}
	// the token decides the exit, not the stale realm field in the document
	if got := e.pool.Get("cn-user"); got == nil || got.Realm != "cn" {
		t.Fatalf("realm from the token: %v", got)
	}
	if e.pool.Get("weird_uid__") == nil && e.pool.Get("weird_uid") == nil {
		t.Errorf("uids are sanitised")
	}
	if over := e.c.Import(rows, "", true, false); len(over.Updated) != 4 && len(over.Updated) != 3 {
		t.Errorf("overwrite updates the existing accounts: %+v", over)
	}
	// a forced exit wins
	forced := e.c.Import(rows[:1], "intl", true, false)
	if got := e.pool.Get("cn-user"); got.Realm != "intl" || len(forced.Updated) != 1 {
		t.Errorf("forced realm: %+v %v", forced, got.Realm)
	}

	out, err := e.c.Export("", []string{"u-1"}, false)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "accessToken") || out["count"].(int) != 1 || out["format"] != ExportFormat {
		t.Fatalf("exports without secrets carry no tokens: %s", b)
	}
	full, _ := e.c.Export("", []string{"u-1"}, true)
	b, _ = json.Marshal(full)
	if !strings.Contains(string(b), "accessToken") {
		t.Errorf("export with secrets: %s", b)
	}
	// a document we exported imports again
	round, err := ParseDocument(b)
	if err != nil || len(round) != 1 {
		t.Fatalf("%v", err)
	}
	for _, bad := range []string{`{"foo":1}`, `[]`, `[1]`, `nope`, `"x"`} {
		if _, err := ParseDocument([]byte(bad)); err == nil {
			t.Errorf("%s should be refused", bad)
		}
	}
}

func TestSyncProfile(t *testing.T) {
	e := newEnv(t, "intl", nil)
	e.vendor.json("/console/account", `{"uid":"u-1","nickname":" Real Name ","phoneNumber":"123"}`)
	if res := e.c.SyncProfile(context.Background(), e.acc()); !res.OK || e.acc().Record().Nickname != "Real Name" {
		t.Fatalf("%+v %q", res, e.acc().Record().Nickname)
	}
	e.vendor.json("/console/account", `{"uid":"someone-else","nickname":"X"}`)
	if res := e.c.SyncProfile(context.Background(), e.acc()); res.OK {
		t.Fatal("a uid mismatch is the wrong account")
	}
}
