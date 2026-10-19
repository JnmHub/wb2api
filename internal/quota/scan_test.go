package quota

import (
	"context"
	"testing"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/store/accounts"
)

type fakeScanProber struct {
	calls  int
	ok     bool
	reset  time.Time
	detail string
}

func (f *fakeScanProber) ProbeModel(ctx context.Context, acc *pool.Account, model string) (bool, time.Time, string) {
	f.calls++
	return f.ok, f.reset, f.detail
}

func scanPool(t *testing.T, uids ...string) *pool.Pool {
	t.Helper()
	var recs []*accounts.Record
	exp := time.Now().Add(24 * time.Hour).Unix()
	for _, uid := range uids {
		recs = append(recs, accounts.FromMap(map[string]any{
			"uid": uid, "accessToken": "tok", "realm": "intl", "expiresAt": exp, "enabled": true,
		}, "", time.Now()))
	}
	return pool.New(recs)
}

func TestScanSeedsAndRespectsTheDueTime(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	pr := &fakeScanProber{ok: true}
	s := &Scanner{Dir: t.TempDir(), Now: func() time.Time { return now }, Prober: pr,
		Models: func() []string { return []string{"m1"} }}
	p := scanPool(t, "u1", "u2")
	if sent := s.RunDue(context.Background(), p, false); sent != 0 {
		t.Fatalf("nothing is due right after seeding: %d", sent)
	}
	if len(s.Snapshot()) != 2 {
		t.Fatalf("rows: %+v", s.Snapshot())
	}
	now = now.Add(10 * time.Second) // the first pair was staggered by 5 s
	if sent := s.RunDue(context.Background(), p, false); sent != 1 {
		t.Fatalf("one pair is due: %d", sent)
	}
	if pr.calls != 1 {
		t.Fatalf("calls %d", pr.calls)
	}
	rows := s.Snapshot()
	var ok int
	for _, r := range rows {
		if r.LastResult == "ok" {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("rows: %+v", rows)
	}
}

func TestScan429HandsTheLineToTheWindowProbe(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Hour)
	pr := &fakeScanProber{reset: reset, detail: "HTTP 429: limited"}
	dir := t.TempDir()
	w := &Windows{Dir: dir, Now: func() time.Time { return now }}
	s := &Scanner{Dir: dir, Now: func() time.Time { return now }, Prober: pr,
		Models: func() []string { return []string{"m1"} }, Windows: w}
	p := scanPool(t, "u1")
	s.RunDue(context.Background(), p, true)
	if !w.Has("u1", "m1") {
		t.Fatal("a 429 with a reset clock must register a window")
	}
	rows := s.Snapshot()
	if len(rows) != 1 || rows[0].LastResult != "throttled" || rows[0].Attempts != 0 {
		t.Fatalf("rows: %+v", rows)
	}
	// The window owns the line now, so no further scan is sent.
	before := pr.calls
	now = now.Add(7 * time.Hour)
	if sent := s.RunDue(context.Background(), p, true); sent != 0 || pr.calls != before {
		t.Fatalf("a windowed line must not be scanned: sent=%d calls=%d", sent, pr.calls)
	}
}

func TestScanRetriesThenDefersAFullInterval(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	pr := &fakeScanProber{detail: "boom"}
	s := &Scanner{Dir: t.TempDir(), Now: func() time.Time { return now }, Prober: pr,
		Models: func() []string { return []string{"m1"} }}
	p := scanPool(t, "u1")
	for i := 0; i < scanMaxAttempt; i++ {
		now = now.Add(16 * time.Minute)
		s.RunDue(context.Background(), p, true)
	}
	rows := s.Snapshot()
	if len(rows) != 1 {
		t.Fatalf("rows: %+v", rows)
	}
	if rows[0].LastResult != "error" || rows[0].Attempts != 0 || rows[0].LastScan == 0 {
		t.Fatalf("after the ladder the row should defer a full interval: %+v", rows[0])
	}
	if pr.calls != scanMaxAttempt {
		t.Fatalf("calls %d", pr.calls)
	}
}

func TestScanOffWhenNoModelsAreWatched(t *testing.T) {
	pr := &fakeScanProber{ok: true}
	s := &Scanner{Dir: t.TempDir(), Prober: pr, Models: func() []string { return nil }}
	if sent := s.RunDue(context.Background(), scanPool(t, "u1"), true); sent != 0 || pr.calls != 0 {
		t.Fatalf("sent=%d calls=%d", sent, pr.calls)
	}
}
