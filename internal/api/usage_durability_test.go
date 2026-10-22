package api

import (
	"strings"
	"testing"
	"time"

	"wb2api/internal/usagedb"
)

func TestSummaryDurabilitySeparatesSinkFailureFromReadiness(t *testing.T) {
	u, mux := newUsage(t, time.Now())
	if _, err := u.DB.SQL().Exec("CREATE TRIGGER reject_api_usage BEFORE INSERT ON requests BEGIN SELECT RAISE(ABORT, 'injected sink failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := u.DB.Record(usagedb.Event{At: time.Now(), Model: "m", Outcome: "completed", Total: 1}); err != nil {
		t.Fatal(err)
	}
	if err := u.DB.Flush(); err == nil {
		t.Fatal("injected sink failure did not retain pending event")
	}
	code, out := get(t, mux, "/api/v2/usage/summary?range=all")
	if code != 200 {
		t.Fatalf("summary failed: %d %v", code, out)
	}
	d := out["durability"].(map[string]any)
	if d["ready"] != true || d["pending_events"] != float64(1) || !strings.Contains(d["sink_error"].(string), "injected sink failure") || d["durability_error"] != "" {
		t.Fatalf("sink failure confused with local durability: %v", d)
	}
	if _, err := u.DB.SQL().Exec("DROP TRIGGER reject_api_usage"); err != nil {
		t.Fatal(err)
	}
	if err := u.DB.Flush(); err != nil {
		t.Fatal(err)
	}
	_, out = get(t, mux, "/api/v2/usage/summary?range=all")
	d = out["durability"].(map[string]any)
	if d["ready"] != true || d["pending_events"] != float64(0) || d["sink_error"] != "" {
		t.Fatalf("recovery not exposed: %v", d)
	}
}
