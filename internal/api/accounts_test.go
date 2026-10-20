package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wb2api/internal/events"
	"wb2api/internal/jobs"
	"wb2api/internal/pool"
	"wb2api/internal/refresh"
	"wb2api/internal/store/accounts"
	"wb2api/internal/usagedb"
)

type fakeRefresher struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeRefresher) Refresh(ctx context.Context, a *pool.Account) error {
	f.mu.Lock()
	f.calls = append(f.calls, a.UID)
	f.mu.Unlock()
	if a.UID == "acct-0003" {
		return fmt.Errorf("upstream said no")
	}
	return nil
}
func (f *fakeRefresher) StatusOf(uid string) refresh.Status { return refresh.Status{} }

type fixture struct {
	acc       *Accounts
	mux       *http.ServeMux
	persisted map[string]*accounts.Record
	deleted   []string
	pmu       sync.Mutex
	fr        *fakeRefresher
}

func newFixture(t *testing.T, n int) *fixture {
	t.Helper()
	exp := time.Now().Add(24 * time.Hour).Unix()
	recs := make([]*accounts.Record, n)
	for i := range recs {
		realmName := "intl"
		if i%4 == 3 {
			realmName = "cn"
		}
		recs[i] = &accounts.Record{
			UID: fmt.Sprintf("acct-%04d", i), Nickname: fmt.Sprintf("Nick%d", i), Realm: realmName, AccessToken: "t",
			RefreshToken: "r", ExpiresAt: exp, Weight: 1, Product: "workbuddy", AddedAt: float64(1_790_000_000 + i),
		}
	}
	f := &fixture{persisted: map[string]*accounts.Record{}, fr: &fakeRefresher{}}
	f.acc = &Accounts{
		Pool: pool.New(recs), Today: &Today{}, Jobs: &jobs.Manager{Workers: 4}, Bus: events.New(64), Refresher: f.fr,
		Persist: func(r *accounts.Record) error { f.pmu.Lock(); f.persisted[r.UID] = r; f.pmu.Unlock(); return nil },
		Delete: func(r *accounts.Record) error {
			f.pmu.Lock()
			f.deleted = append(f.deleted, r.UID)
			f.pmu.Unlock()
			return nil
		},
	}
	f.mux = http.NewServeMux()
	f.acc.Register(f.mux, func(h http.Handler) http.Handler { return h })
	return f
}

func (f *fixture) do(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func uids(out map[string]any) []string {
	var r []string
	for _, it := range out["items"].([]any) {
		r = append(r, it.(map[string]any)["uid"].(string))
	}
	return r
}

func TestListPagesFiltersAndSorts(t *testing.T) {
	f := newFixture(t, 40)
	all := f.acc.Pool.All()
	all[1].SetEnabled(false)
	all[2].CoolDown(time.Now().Add(time.Hour))
	all[5].MarkTokenBad(true)
	all[7].NoteRestricted(time.Now(), time.Hour)

	code, out := f.do(t, "GET", "/api/v2/accounts?limit=10", "")
	if code != 200 || out["total"].(float64) != 40 || len(out["items"].([]any)) != 10 {
		t.Fatalf("%d %v", code, out)
	}
	// page 2 continues where page 1 stopped, no overlap
	_, p1 := f.do(t, "GET", "/api/v2/accounts?limit=10&offset=0&sort=name", "")
	_, p2 := f.do(t, "GET", "/api/v2/accounts?limit=10&offset=10&sort=name", "")
	seen := map[string]bool{}
	for _, u := range append(uids(p1), uids(p2)...) {
		if seen[u] {
			t.Fatalf("duplicate across pages: %s", u)
		}
		seen[u] = true
	}
	// the default order sinks disabled accounts
	_, def := f.do(t, "GET", "/api/v2/accounts?limit=500", "")
	ids := uids(def)
	if ids[len(ids)-1] != "acct-0001" {
		t.Errorf("the disabled account must come last, got %v", ids[len(ids)-3:])
	}

	for status, want := range map[string]string{"disabled": "acct-0001", "cooling": "acct-0002", "need_login": "acct-0005", "restricted": "acct-0007"} {
		_, o := f.do(t, "GET", "/api/v2/accounts?status="+status, "")
		if got := uids(o); len(got) != 1 || got[0] != want {
			t.Errorf("status=%s -> %v, want [%s]", status, got, want)
		}
	}
	_, o := f.do(t, "GET", "/api/v2/accounts?status=ready&realm=cn", "")
	if o["total"].(float64) != 10-1 && o["total"].(float64) != 10 { // cn accounts are every 4th: 10 of them, #7 is restricted
		t.Errorf("ready cn: %v", o["total"])
	}
	_, o = f.do(t, "GET", "/api/v2/accounts?q=nick12", "")
	if got := uids(o); len(got) != 1 || got[0] != "acct-0012" {
		t.Errorf("search by nickname: %v", got)
	}
	_, o = f.do(t, "GET", "/api/v2/accounts?q=ACCT-0033", "")
	if got := uids(o); len(got) != 1 || got[0] != "acct-0033" {
		t.Errorf("search by uid is case-insensitive: %v", got)
	}
	_, o = f.do(t, "GET", "/api/v2/accounts?sort=added&order=desc&limit=1", "")
	if got := uids(o); got[0] != "acct-0039" {
		t.Errorf("sort by added desc: %v", got)
	}

	for _, bad := range []string{"?status=bogus", "?sort=password"} {
		if code, _ := f.do(t, "GET", "/api/v2/accounts"+bad, ""); code != 400 {
			t.Errorf("%s -> %d", bad, code)
		}
	}
	if _, o := f.do(t, "GET", "/api/v2/accounts?offset=9999", ""); len(o["items"].([]any)) != 0 || o["offset"].(float64) != 40 {
		t.Errorf("offset past the end: %v", o)
	}
}

func TestSortByTodaysUsage(t *testing.T) {
	f := newFixture(t, 5)
	f.acc.Today.Add(usagedb.Event{At: time.Now(), Account: "acct-0002", Total: 500, Credit: 2})
	f.acc.Today.Add(usagedb.Event{At: time.Now(), Account: "acct-0004", Total: 900})
	f.acc.Today.Add(usagedb.Event{At: time.Now(), Account: "acct-0004", Total: 100})
	_, o := f.do(t, "GET", "/api/v2/accounts?sort=tokens&order=desc&limit=2", "")
	if got := uids(o); got[0] != "acct-0004" || got[1] != "acct-0002" {
		t.Fatalf("%v", got)
	}
	item := o["items"].([]any)[0].(map[string]any)["today"].(map[string]any)
	if item["tokens"].(float64) != 1000 || item["requests"].(float64) != 2 {
		t.Fatalf("%v", item)
	}
}

func TestSummary(t *testing.T) {
	f := newFixture(t, 8)
	f.acc.Pool.All()[0].SetEnabled(false)
	f.acc.Pool.All()[1].CoolDown(time.Now().Add(time.Hour))
	_, o := f.do(t, "GET", "/api/v2/accounts/summary", "")
	st := o["by_status"].(map[string]any)
	if o["total"].(float64) != 8 || st["disabled"].(float64) != 1 || st["cooling"].(float64) != 1 || st["ready"].(float64) != 6 {
		t.Fatalf("%v", o)
	}
}

func TestDetailAndPatchPersists(t *testing.T) {
	f := newFixture(t, 3)
	code, d := f.do(t, "GET", "/api/v2/accounts/acct-0001", "")
	if code != 200 || d["machine_id"] == "" || d["session_id"] == "" {
		t.Fatalf("%d %v", code, d)
	}
	if code, _ := f.do(t, "GET", "/api/v2/accounts/nope", ""); code != 404 {
		t.Fatal("unknown account")
	}
	code, out := f.do(t, "PATCH", "/api/v2/accounts/acct-0001", `{"weight":7,"note":"  batch  ","enabled":false,"disabled_models":["GPT-5.5"," gpt-5.5","X"],"product":"CLI"}`)
	if code != 200 || out["weight"].(float64) != 7 || out["note"] != "batch" || out["enabled"] != false || out["product"] != "cli" {
		t.Fatalf("%d %v", code, out)
	}
	rec := f.persisted["acct-0001"]
	if rec == nil || rec.Weight != 7 || rec.Note != "batch" || rec.IsEnabled() || len(rec.DisabledModels) != 2 || rec.DisabledModels[0] != "gpt-5.5" {
		t.Fatalf("not persisted correctly: %+v", rec)
	}
	acc := f.acc.Pool.Get("acct-0001")
	if acc.Ready("gpt-5.5", time.Now()) || acc.Weight() != 7 {
		t.Fatal("the live account must reflect the patch immediately")
	}
	for _, bad := range []string{`{"weight":0}`, `{"weight":101}`, `{"product":"mystery"}`, `{"nope":1}`, `not json`, `{"note":"` + strings.Repeat("x", 201) + `"}`} {
		if code, _ := f.do(t, "PATCH", "/api/v2/accounts/acct-0001", bad); code != 400 {
			t.Errorf("%q -> %d, want 400", bad, code)
		}
	}
	if code, _ := f.do(t, "PATCH", "/api/v2/accounts/nope", `{"weight":2}`); code != 404 {
		t.Fatal("patch unknown")
	}
}

func TestDelete(t *testing.T) {
	f := newFixture(t, 3)
	if code, _ := f.do(t, "DELETE", "/api/v2/accounts/acct-0001", ""); code != 200 {
		t.Fatal("delete")
	}
	if f.acc.Pool.Len() != 2 || f.acc.Pool.Get("acct-0001") != nil || len(f.deleted) != 1 {
		t.Fatalf("pool=%d deleted=%v", f.acc.Pool.Len(), f.deleted)
	}
	if code, _ := f.do(t, "DELETE", "/api/v2/accounts/acct-0001", ""); code != 404 {
		t.Fatal("second delete")
	}
}

func waitJob(t *testing.T, f *fixture, id string) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if j, ok := f.acc.Jobs.Get(id); ok && j.State != jobs.Running {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return jobs.Job{}
}

func TestBatchByUIDsAndByFilter(t *testing.T) {
	f := newFixture(t, 30)
	code, out := f.do(t, "POST", "/api/v2/accounts/batch", `{"action":"disable","uids":["acct-0001","acct-0002","ghost"]}`)
	if code != 202 || out["total"].(float64) != 2 {
		t.Fatalf("unknown uids are skipped: %d %v", code, out)
	}
	j := waitJob(t, f, out["id"].(string))
	if j.State != jobs.Done || j.Done != 2 || j.Failed != 0 {
		t.Fatalf("%+v", j)
	}
	if f.acc.Pool.Get("acct-0001").Enabled() || f.persisted["acct-0001"] == nil {
		t.Fatal("not applied / persisted")
	}

	// "everything matching the filter", not just the visible page
	code, out = f.do(t, "POST", "/api/v2/accounts/batch", `{"action":"set_weight","filter":{"realm":"cn"},"params":{"weight":5}}`)
	if code != 202 {
		t.Fatalf("%d %v", code, out)
	}
	waitJob(t, f, out["id"].(string))
	for _, a := range f.acc.Pool.All() {
		want := 1
		if a.Realm == "cn" {
			want = 5
		}
		if a.Weight() != want {
			t.Fatalf("%s weight %d, want %d", a.UID, a.Weight(), want)
		}
	}
}

func TestBatchRulesAndErrors(t *testing.T) {
	f := newFixture(t, 6)
	cases := []struct {
		name, body string
		want       int
	}{
		{"unknown action", `{"action":"explode","uids":["acct-0001"]}`, 400},
		{"delete needs confirm", `{"action":"delete","uids":["acct-0001"]}`, 400},
		{"clearing a restriction needs confirm", `{"action":"clear_restriction","uids":["acct-0001"]}`, 400},
		{"weight out of range", `{"action":"set_weight","uids":["acct-0001"],"params":{"weight":500}}`, 400},
		{"no target", `{"action":"enable"}`, 400},
		{"uids and filter", `{"action":"enable","uids":["acct-0001"],"filter":{}}`, 400},
		{"nothing matches", `{"action":"enable","filter":{"q":"zzzzzz"}}`, 400},
		{"vendor actions need the ops client", `{"action":"checkin","uids":["acct-0001"]}`, 400},
		{"bad product", `{"action":"set_product","uids":["acct-0001"],"params":{"product":"x"}}`, 400},
	}
	for _, c := range cases {
		if code, out := f.do(t, "POST", "/api/v2/accounts/batch", c.body); code != c.want {
			t.Errorf("%s: %d %v, want %d", c.name, code, out, c.want)
		}
	}
	if f.acc.Pool.Len() != 6 {
		t.Fatal("rejected requests must not change anything")
	}
}

func TestBatchRefreshTokenReportsFailures(t *testing.T) {
	f := newFixture(t, 6)
	code, out := f.do(t, "POST", "/api/v2/accounts/batch", `{"action":"refresh_token","filter":{}}`)
	if code != 202 {
		t.Fatalf("%d %v", code, out)
	}
	j := waitJob(t, f, out["id"].(string))
	if j.Total != 6 || j.Failed != 1 || len(j.Errors) != 1 || j.Errors[0].Item != "acct-0003" || !strings.Contains(j.Errors[0].Error, "upstream said no") {
		t.Fatalf("%+v", j)
	}
}

func TestClearRestrictionPersists(t *testing.T) {
	f := newFixture(t, 2)
	acc := f.acc.Pool.All()[0]
	acc.NoteRestricted(time.Now(), time.Hour)
	acc.CoolDown(time.Now().Add(time.Hour))
	code, out := f.do(t, "POST", "/api/v2/accounts/batch", `{"action":"clear_restriction","uids":["acct-0000"],"confirm":true}`)
	if code != 202 {
		t.Fatal(code)
	}
	waitJob(t, f, out["id"].(string))
	if !acc.RestrictedUntil().IsZero() || acc.State(time.Now()) != pool.StatusReady {
		t.Fatalf("state %s", acc.State(time.Now()))
	}
	if r := f.persisted["acct-0000"]; r == nil || r.RestrictedUntil != 0 {
		t.Fatal("lifting a restriction must be written to the account file")
	}
}

func TestJobEndpointsAndCancel(t *testing.T) {
	f := newFixture(t, 4)
	_, out := f.do(t, "POST", "/api/v2/accounts/batch", `{"action":"enable","filter":{}}`)
	id := out["id"].(string)
	waitJob(t, f, id)
	if code, j := f.do(t, "GET", "/api/v2/jobs/"+id, ""); code != 200 || j["state"] != "done" {
		t.Fatalf("%d %v", code, j)
	}
	if code, l := f.do(t, "GET", "/api/v2/jobs", ""); code != 200 || len(l["jobs"].([]any)) != 1 {
		t.Fatalf("%d %v", code, l)
	}
	if code, _ := f.do(t, "GET", "/api/v2/jobs/none", ""); code != 404 {
		t.Fatal("unknown job")
	}
	if code, _ := f.do(t, "DELETE", "/api/v2/jobs/none", ""); code != 404 {
		t.Fatal("cancel unknown job")
	}
}

func TestChangesAreBatchedIntoOneEvent(t *testing.T) {
	f := newFixture(t, 5)
	sub := f.acc.Bus.Subscribe(0)
	defer sub.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.acc.StartEvents(ctx, 20*time.Millisecond)
	a := f.acc.Pool.All()[2]
	for i := 0; i < 50; i++ { // a burst of changes to one account...
		a.CoolDown(time.Now().Add(time.Duration(i+1) * time.Minute))
	}
	f.acc.Pool.All()[3].SetEnabled(false) // ...and one to another
	select {
	case ev := <-sub.C:
		if ev.Type != "account.patch" {
			t.Fatalf("%+v", ev)
		}
		var p struct {
			Accounts []map[string]any `json:"accounts"`
		}
		_ = json.Unmarshal(ev.Data, &p)
		if len(p.Accounts) != 2 {
			t.Fatalf("one message with both changed accounts expected, got %d: %s", len(p.Accounts), ev.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no account.patch event")
	}
	select {
	case ev := <-sub.C:
		t.Fatalf("a burst must produce a single event, also got %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
	// removal is announced as well
	f.acc.Pool.Remove("acct-0001")
	select {
	case ev := <-sub.C:
		if !bytes.Contains(ev.Data, []byte(`"removed":["acct-0001"]`)) {
			t.Fatalf("%s", ev.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("removal not announced")
	}
}

// 5,000 accounts: a filtered, sorted page must stay fast (the legacy panel
// re-rendered every account every 15 s).
func TestListAtScale(t *testing.T) {
	f := newFixture(t, 5000)
	for i, a := range f.acc.Pool.All() {
		if i%7 == 0 {
			a.CoolDown(time.Now().Add(time.Hour))
		}
	}
	start := time.Now()
	const rounds = 20
	for i := 0; i < rounds; i++ {
		code, o := f.do(t, "GET", "/api/v2/accounts?status=ready&sort=name&limit=50&q=nick", "")
		if code != 200 || len(o["items"].([]any)) != 50 {
			t.Fatalf("%d", code)
		}
	}
	per := time.Since(start) / rounds
	t.Logf("filtered+sorted page of 50 over 5000 accounts: %v per request", per)
	if per > 150*time.Millisecond {
		t.Errorf("too slow: %v", per)
	}
}

func TestSummaryCredits(t *testing.T) {
	f := newFixture(t, 1)
	acc := f.acc.Pool.All()[0]
	rec := *acc.Record()
	rec.Credits = map[string]any{"remain": 12.5, "packages": []any{
		map[string]any{"days_left": 2, "remain": 4},
		map[string]any{"days_left": 30, "remain": 8.5},
	}}
	acc.SetRecord(&rec)
	code, out := f.do(t, "GET", "/api/v2/accounts/summary", "")
	if code != http.StatusOK {
		t.Fatalf("summary: %d %v", code, out)
	}
	cr, _ := out["credits"].(map[string]any)
	if cr == nil || cr["remain"].(float64) != 12.5 || cr["available"].(float64) != 12.5 || cr["expiring_7d"].(float64) != 4 {
		t.Fatalf("credits = %#v", cr)
	}
	if cr["zero"].(float64) != 0 {
		t.Fatalf("zero = %v", cr["zero"])
	}
	rec.Credits = map[string]any{"remain": 0}
	acc.SetRecord(&rec)
	_, out = f.do(t, "GET", "/api/v2/accounts/summary", "")
	cr = out["credits"].(map[string]any)
	if cr["zero"].(float64) != 1 || cr["available"].(float64) != 0 {
		t.Fatalf("exhausted summary = %#v", cr)
	}
	st := out["by_status"].(map[string]any)
	if st["no_credit"].(float64) != 1 {
		t.Fatalf("status = %#v", st)
	}
}

func TestBatchRealmFilter(t *testing.T) {
	f := newFixture(t, 4)
	all := []string{}
	for _, a := range f.acc.Pool.All() {
		all = append(all, a.UID)
	}
	got, msg := f.acc.filterBatchRealm("checkin", all)
	if len(got) != 1 || got[0] != "acct-0003" || msg != "" {
		t.Fatalf("checkin filter = %v %q", got, msg)
	}
	got, msg = f.acc.filterBatchRealm("daily_chat", all)
	if len(got) != 3 || msg != "" {
		t.Fatalf("daily filter = %v %q", got, msg)
	}
	got, msg = f.acc.filterBatchRealm("checkin", all[:3])
	if len(got) != 0 || msg == "" {
		t.Fatalf("empty checkin filter = %v %q", got, msg)
	}
}
