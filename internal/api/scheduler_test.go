package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/sched"
	"wb2api/internal/store/accounts"
)

func TestSchedulerSummaryAggregatesLiveState(t *testing.T) {
	exp := time.Now().Add(24 * time.Hour).Unix()
	p := pool.New([]*accounts.Record{
		{UID: "acct-0001", Nickname: "fast", Realm: "intl", AccessToken: "t", ExpiresAt: exp, Weight: 2},
		{UID: "acct-0002", Nickname: "slow", Realm: "cn", AccessToken: "t", ExpiresAt: exp, Weight: 1},
	})
	p.Get("acct-0001").SetMaxInflight(4)
	p.Get("acct-0002").SetMaxInflight(2)
	p.Get("acct-0001").Acquire()
	p.Get("acct-0002").Acquire()
	p.Get("acct-0002").Acquire()

	feedback := sched.NewStats(0, 0)
	feedback.Observe("acct-0001", 400*time.Millisecond, false)
	feedback.Observe("acct-0002", 2*time.Second, false)
	feedback.Observe("acct-0002", 0, true)

	mux := http.NewServeMux()
	(&Scheduler{
		Pool: p, Feedback: feedback, TopK: p.TopK,
		Score: func(a *pool.Account) int64 { return int64(a.Inflight()) },
		Realm: func() string { return "intl" },
	}).Register(mux, func(h http.Handler) http.Handler { return h })

	req := httptest.NewRequest("GET", "/api/v2/scheduler/summary", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["realm"] != "intl" {
		t.Fatalf("realm = %v", out["realm"])
	}
	if out["total"].(float64) != 2 || out["inflight"].(float64) != 3 || out["capacity"].(float64) != 6 {
		t.Fatalf("totals = %v", out)
	}
	if out["tracked"].(float64) != 2 {
		t.Fatalf("tracked = %v", out["tracked"])
	}
	hot := out["hot"].([]any)
	if len(hot) != 2 {
		t.Fatalf("hot rows = %d", len(hot))
	}
	first := hot[0].(map[string]any)
	if first["uid"] != "acct-0002" || first["state"] != "ready" || first["max_inflight"].(float64) != 2 {
		t.Fatalf("first hot row = %v", first)
	}
	states := out["states"].(map[string]any)
	if states["ready"].(float64) != 2 {
		t.Fatalf("states = %v", states)
	}
}
