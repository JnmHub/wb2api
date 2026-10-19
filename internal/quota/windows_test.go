package quota

import (
	"context"
	"testing"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/store/accounts"
)

type fakeProber struct {
	calls  int
	ok     bool
	reset  time.Time
	detail string
}

func (f *fakeProber) ProbeModel(ctx context.Context, acc *pool.Account, model string) (bool, time.Time, string) {
	f.calls++
	return f.ok, f.reset, f.detail
}

func testPool(t *testing.T, uids ...string) *pool.Pool {
	t.Helper()
	var recs []*accounts.Record
	for _, uid := range uids {
		recs = append(recs, accounts.FromMap(map[string]any{
			"uid": uid, "accessToken": "tok-" + uid, "realm": "intl",
			"expiresAt": time.Now().Add(24 * time.Hour).Unix(), "enabled": true,
		}, "", time.Now()))
	}
	return pool.New(recs)
}

func TestScheduleDedupesTheSameResetInstant(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	w := &Windows{Dir: t.TempDir(), Now: func() time.Time { return now }}
	reset := now.Add(time.Hour)
	if !w.Schedule("u1", "hy3", reset) {
		t.Fatal("first schedule must register")
	}
	if w.Schedule("u1", "hy3", reset.Add(500*time.Millisecond)) {
		t.Fatal("the same window must not be rewritten")
	}
	if !w.Has("u1", "hy3") {
		t.Fatal("window must be visible")
	}
	if !w.Schedule("u1", "hy3", reset.Add(time.Hour)) {
		t.Fatal("a new window must replace the old one")
	}
	if w.Schedule("u1", "hy3", now.Add(-time.Second)) {
		t.Fatal("a reset instant in the past is not a window")
	}
}

func TestRunDueSuccessClearsTheWindow(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	p := testPool(t, "u1")
	w := &Windows{Dir: t.TempDir(), Now: func() time.Time { return now }, Prober: &fakeProber{ok: true}}
	w.Schedule("u1", "hy3", now.Add(time.Minute))
	now = now.Add(2 * time.Minute)
	if sent := w.RunDue(context.Background(), p, 8); sent != 1 {
		t.Fatalf("sent %d", sent)
	}
	if w.Has("u1", "hy3") {
		t.Fatal("a reopened window must be dropped")
	}
}

func TestRunDueRetriesThenGivesUp(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	p := testPool(t, "u1")
	pr := &fakeProber{detail: "HTTP 429: not yet"}
	w := &Windows{Dir: t.TempDir(), Now: func() time.Time { return now }, Prober: pr}
	w.Schedule("u1", "hy3", now.Add(time.Minute))
	for i := 0; i < MaxAttempts; i++ {
		now = now.Add(time.Minute)
		w.RunDue(context.Background(), p, 8)
		// the retry clock is 15 s after the (fixed) probe instant, so move past it
		now = now.Add(RetryDelay)
	}
	if w.Has("u1", "hy3") {
		t.Fatal("a window that never reopened must be abandoned after MaxAttempts")
	}
	if pr.calls != MaxAttempts {
		t.Fatalf("probes %d, want %d", pr.calls, MaxAttempts)
	}
}

func TestRunDueReschedulesOnAFreshResetClock(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	p := testPool(t, "u1")
	newReset := now.Add(3 * time.Hour)
	w := &Windows{Dir: t.TempDir(), Now: func() time.Time { return now }, Prober: &fakeProber{reset: newReset, detail: "still limited"}}
	w.Schedule("u1", "hy3", now.Add(time.Minute))
	now = now.Add(2 * time.Minute)
	w.RunDue(context.Background(), p, 8)
	rows := w.Snapshot()
	got := time.Unix(0, int64(rows[0].ResetAt*1e9))
	if len(rows) != 1 || rows[0].Attempts != 0 || !got.Equal(newReset) {
		t.Fatalf("a fresh reset clock must replace the row: %+v", rows)
	}
}

func TestRunDueDropsRecordsOfGoneAccounts(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	p := testPool(t) // empty pool
	w := &Windows{Dir: t.TempDir(), Now: func() time.Time { return now }, Prober: &fakeProber{ok: true}}
	w.Schedule("gone", "hy3", now.Add(time.Minute))
	now = now.Add(2 * time.Minute)
	if sent := w.RunDue(context.Background(), p, 8); sent != 0 {
		t.Fatalf("sent %d", sent)
	}
	if w.Has("gone", "hy3") {
		t.Fatal("the record of an account that left the pool must be dropped")
	}
}

func TestGateOffDisablesEverything(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	w := &Windows{Dir: t.TempDir(), Now: func() time.Time { return now }, Gate: func() bool { return false }, Prober: &fakeProber{ok: true}}
	if w.Schedule("u1", "hy3", now.Add(time.Hour)) {
		t.Fatal("the switch must stop scheduling")
	}
	if w.Has("u1", "hy3") {
		t.Fatal("nothing may be recorded while off")
	}
}

func TestSecondsUntilDue(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	w := &Windows{Dir: t.TempDir(), Now: func() time.Time { return now }}
	if _, ok := w.SecondsUntilDue(); ok {
		t.Fatal("no windows means no due time")
	}
	w.Schedule("u1", "hy3", now.Add(90*time.Second))
	if d, ok := w.SecondsUntilDue(); !ok || d != 90*time.Second {
		t.Fatalf("due in %v ok=%v", d, ok)
	}
}
