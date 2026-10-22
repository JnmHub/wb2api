package api

import (
	"path/filepath"
	"testing"
	"time"

	"wb2api/internal/usagedb"
)

func TestTodaySeedRestoresDurablePendingAcrossRestart(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.Local)
	path := filepath.Join(t.TempDir(), "usage.db")
	opts := usagedb.Options{QueueSize: 1}
	db, err := usagedb.OpenWithOptions(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if db != nil {
			_ = db.Close()
		}
	}()
	record := func(at time.Time, total int64, credit float64) {
		t.Helper()
		if err := db.Record(usagedb.Event{At: at, Model: "m", Account: "a", Total: total, Credit: credit, Outcome: "completed"}); err != nil {
			t.Fatal(err)
		}
	}
	record(now, 10, 0.25)
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	// Fail the immutable ledger, not optional details: even detail-shed events
	// stay pending. Restart must seed them without relying on delivery recovery.
	if _, err := db.SQL().Exec("CREATE TRIGGER reject_today_ledger BEFORE INSERT ON usage_facts BEGIN SELECT RAISE(ABORT, 'sink remains unavailable'); END"); err != nil {
		t.Fatal(err)
	}
	record(now.Add(time.Minute), 20, 0.5)
	record(now.Add(2*time.Minute), 30, 0.75)
	record(now.AddDate(0, 0, -1), 999, 99)
	if err := db.Flush(); err == nil {
		t.Fatal("expected pending sink failure")
	}
	if err := db.Close(); err == nil {
		t.Fatal("expected retained pending work on Close")
	}
	db = nil
	db, err = usagedb.OpenWithOptions(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	want := DayStat{Requests: 3, Tokens: 60, Credit: 1.5}
	check := func() {
		t.Helper()
		today := &Today{Now: func() time.Time { return now.Add(time.Hour) }}
		if err := today.Seed(db); err != nil {
			t.Fatal(err)
		}
		if got := today.Get("a"); got != want {
			t.Fatalf("Today=%+v want %+v", got, want)
		}
	}
	check()
	if h := db.Health(); h.PendingEvents != 3 {
		t.Fatalf("seed unexpectedly required sink delivery: %+v", h)
	}
	if _, err := db.SQL().Exec("DROP TRIGGER reject_today_ledger"); err != nil {
		t.Fatal(err)
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	check() // moving pending to sink cannot change or double-count the seed
}

func TestTodayAddLateYesterdayDoesNotRollBackCurrentDay(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.Local)
	today := &Today{Now: func() time.Time { return now }}
	today.Add(usagedb.Event{At: now, Account: "a", Total: 7, Credit: 0.25})
	today.Add(usagedb.Event{At: now.AddDate(0, 0, -1), Account: "a", Total: 999, Credit: 99})
	if got, want := today.Get("a"), (DayStat{Requests: 1, Tokens: 7, Credit: 0.25}); got != want {
		t.Fatalf("late event reset counters: %+v", got)
	}
	today.Add(usagedb.Event{At: now, Account: "a", Total: 3})
	if got := today.Get("a"); got.Requests != 2 || got.Tokens != 10 {
		t.Fatalf("late event erased prior current-day event: %+v", got)
	}
}

func TestTodaySeedQueryFailurePreservesExistingCounters(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.Local)
	db, err := usagedb.Open(filepath.Join(t.TempDir(), "closed.db"), 16)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	today := &Today{Now: func() time.Time { return now }}
	today.Add(usagedb.Event{At: now, Account: "a", Total: 7, Credit: 0.25})
	before := today.Get("a")
	if err := today.Seed(db); err == nil {
		t.Fatal("closed sink seed must fail")
	}
	if got := today.Get("a"); got != before {
		t.Fatalf("failed seed changed counters: %+v -> %+v", before, got)
	}
}
