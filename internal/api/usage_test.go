package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"wb2api/internal/gateway/openai"
	"wb2api/internal/usagedb"
)

func newUsage(t *testing.T, now time.Time) (*Usage, *http.ServeMux) {
	t.Helper()
	db, err := usagedb.Open(filepath.Join(t.TempDir(), "u.db"), 256)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	u := &Usage{DB: db, Now: func() time.Time { return now }}
	mux := http.NewServeMux()
	u.Register(mux, func(h http.Handler) http.Handler { return h })
	return u, mux
}

func get(t *testing.T, mux *http.ServeMux, path string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestEventFromGateway(t *testing.T) {
	ev := openai.UsageEvent{
		At: time.Now(), Model: "glm-5.3", Account: "u1", KeyID: "k", Stream: true, Outcome: "completed", Status: 200,
		Elapsed: 2500 * time.Millisecond, TTFT: 400 * time.Millisecond, Gen: 2 * time.Second, HeldFor: 30 * time.Millisecond,
		Usage: json.RawMessage(`{"prompt_tokens":100,"completion_tokens":40,"total_tokens":140,"credit":0.25,
		  "completion_tokens_details":{"reasoning_tokens":12},"prompt_tokens_details":{"cached_tokens":64}}`),
	}
	e := EventFromGateway(ev, func(string) string { return "cn" })
	if e.Realm != "cn" || e.Prompt != 100 || e.Completion != 40 || e.Reasoning != 12 || e.Cached != 64 || e.Total != 140 ||
		e.Credit != 0.25 || *e.TTFTMs != 400 || *e.GenMs != 2000 || e.ElapsedMs != 2500 || e.HeldMs != 30 {
		t.Fatalf("%+v", e)
	}
	failed := EventFromGateway(openai.UsageEvent{At: time.Now(), Outcome: "failed", Status: 429}, nil)
	if failed.Error == "" || failed.TTFTMs != nil || failed.GenMs != nil {
		t.Fatalf("%+v", failed)
	}
}

func TestSummarySeriesRecentAndRanges(t *testing.T) {
	now := time.Date(2026, 10, 21, 15, 0, 0, 0, time.Local) // a Wednesday
	u, mux := newUsage(t, now)
	rec := func(daysAgo int, hour int, model, acct string, total int64) {
		at := time.Date(2026, 10, 21-daysAgo, hour, 5, 0, 0, time.Local)
		u.DB.Record(usagedb.Event{At: at, Model: model, Account: acct, Realm: "intl", Outcome: "completed", Total: total, Completion: total / 2})
	}
	rec(0, 9, "m1", "a1", 100)   // today
	rec(0, 10, "m2", "a2", 50)   // today
	rec(1, 9, "m1", "a1", 10)    // Tuesday (this week)
	rec(3, 9, "m1", "a1", 1)     // last Sunday (previous week)
	rec(25, 9, "m1", "a1", 1000) // previous month
	u.DB.Flush()

	totalOf := func(q string) float64 {
		code, out := get(t, mux, "/api/v2/usage/summary"+q)
		if code != 200 {
			t.Fatalf("%s -> %d %v", q, code, out)
		}
		return out["totals"].(map[string]any)["total_tokens"].(float64)
	}
	if got := totalOf("?range=today"); got != 150 {
		t.Errorf("today = %v", got)
	}
	if got := totalOf(""); got != 150 {
		t.Errorf("default range is today, got %v", got)
	}
	if got := totalOf("?range=week"); got != 160 {
		t.Errorf("week starts on Monday: got %v want 160", got)
	}
	if got := totalOf("?range=month"); got != 161 {
		t.Errorf("month: got %v want 161", got)
	}
	if got := totalOf("?range=all"); got != 1161 {
		t.Errorf("all: got %v", got)
	}
	if got := totalOf("?range=custom&since=2026-10-20&until=2026-10-20"); got != 10 {
		t.Errorf("custom single day (until is inclusive): got %v", got)
	}
	if got := totalOf("?range=all&model=m2"); got != 50 {
		t.Errorf("model filter: %v", got)
	}

	code, out := get(t, mux, "/api/v2/usage/summary?range=all&top=1")
	if code != 200 || out["by_model_truncated"] != false && out["by_account_truncated"] != true {
		t.Errorf("top-N truncation flag: %v", out["by_account_truncated"])
	}
	if models := out["by_model"].([]any); models[0].(map[string]any)["key"] != "m1" {
		t.Errorf("biggest consumer first: %v", models)
	}

	code, out = get(t, mux, "/api/v2/usage/series?bucket=day&range=month")
	if code != 200 || len(out["points"].([]any)) != 3 {
		t.Errorf("series: %d %v", code, out)
	}
	code, out = get(t, mux, "/api/v2/usage/recent?range=all&limit=2")
	reqs := out["requests"].([]any)
	if code != 200 || len(reqs) != 2 || out["next_before"].(float64) <= 0 {
		t.Fatalf("recent: %d %v", code, out)
	}
	code, out = get(t, mux, "/api/v2/usage/recent?range=all&limit=10&before="+jsonNum(out["next_before"]))
	if code != 200 || len(out["requests"].([]any)) != 3 {
		t.Errorf("cursor page: %v", out)
	}

	if code, _ := get(t, mux, "/api/v2/usage/summary?range=decade"); code != 400 {
		t.Errorf("bad range: %d", code)
	}
	if code, _ := get(t, mux, "/api/v2/usage/summary?range=custom&since=yesterday"); code != 400 {
		t.Errorf("bad date: %d", code)
	}
	if code, _ := get(t, mux, "/api/v2/usage/series?bucket=week"); code != 400 {
		t.Errorf("bad bucket: %d", code)
	}
}

func jsonNum(v any) string { b, _ := json.Marshal(v); return string(b) }
