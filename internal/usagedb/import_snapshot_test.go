package usagedb

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshotImportRetainsSampledAndAggregateOnlyLedger(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "source.db")
	src, err := OpenWithOptions(path, Options{QueueSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Keep automatic replay blocked, force detail shedding, then recover.
	failModel(t, src, "bad")
	if err = src.Record(retryEvent("bad")); err != nil {
		t.Fatal(err)
	}
	_ = src.Flush()
	for range 20 {
		if err = src.Record(retryEvent("m")); err != nil {
			t.Fatal(err)
		}
	}
	src.sql.Exec("DROP TRIGGER reject_retry")
	if err = src.Close(); err != nil {
		t.Fatal(err)
	}
	source, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	// Simulate pre-upgrade aggregate-only history in the source.
	if _, err = source.Exec("UPDATE daily SET requests=requests+9,total=total+63,credit=credit+2.25 WHERE model='bad'; UPDATE hourly SET requests=requests+9,total=total+63,credit=credit+2.25 WHERE model='bad'"); err != nil {
		t.Fatal(err)
	}
	target := open(t)
	if err = target.Record(retryEvent("existing")); err != nil {
		t.Fatal(err)
	}
	if err = target.Flush(); err != nil {
		t.Fatal(err)
	}
	n, err := target.importSQLiteSnapshot(ctx, source, path, true)
	if err != nil || n != 1 {
		t.Fatalf("snapshot=%d %v", n, err)
	}
	for range 2 {
		a, err := target.Totals(Filter{})
		if err != nil || a.Requests != 31 || a.Total != 217 || a.Credit != 7.75 {
			t.Fatalf("aggregate=%+v %v", a, err)
		}
		points, err := target.PairPointsContext(ctx, retryEvent("x").At.AddDate(-1, 0, 0), retryEvent("x").At.AddDate(1, 0, 0))
		if err != nil || len(points) != 22 {
			t.Fatalf("facts=%d %v", len(points), err)
		}
		if err = target.RebuildRollups(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n, err = target.importSQLiteSnapshot(ctx, source, path, true); err != nil || n != 0 {
		t.Fatalf("replay=%d %v", n, err)
	}
	source.Exec("UPDATE daily SET total=total+1")
	if _, err = target.importSQLiteSnapshot(ctx, source, path, true); err == nil || !strings.Contains(err.Error(), "changed after final") {
		t.Fatalf("mutated snapshot=%v", err)
	}
	a, err := target.Totals(Filter{})
	if err != nil || a.Total != 217 {
		t.Fatalf("mutation changed target=%+v %v", a, err)
	}
}
func TestSnapshotImportFailureRollsBackReceiptAndAllTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.db")
	src, err := Open(path, 32)
	if err != nil {
		t.Fatal(err)
	}
	if err = src.Record(retryEvent("m")); err != nil {
		t.Fatal(err)
	}
	if err = src.Close(); err != nil {
		t.Fatal(err)
	}
	source, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target := open(t)
	target.sql.Exec("CREATE TRIGGER fail_snapshot BEFORE INSERT ON hourly BEGIN SELECT RAISE(ABORT,'injected snapshot fault'); END")
	if _, err = target.importSQLiteSnapshot(context.Background(), source, path, true); err == nil {
		t.Fatal("expected fault")
	}
	checkRetryTotals(t, target, 0)
	var receipt int
	if err = target.sql.QueryRow("SELECT COUNT(*) FROM meta WHERE k LIKE 'import-snapshot:%'").Scan(&receipt); err != nil || receipt != 0 {
		t.Fatalf("receipt=%d %v", receipt, err)
	}
	target.sql.Exec("DROP TRIGGER fail_snapshot")
	if n, err := target.importSQLiteSnapshot(context.Background(), source, path, true); err != nil || n != 1 {
		t.Fatalf("retry=%d %v", n, err)
	}
	checkRetryTotals(t, target, 1)
}
func TestSnapshotImportRejectsLiveAndPendingSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.db")
	src, err := Open(path, 32)
	if err != nil {
		t.Fatal(err)
	}
	source, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target := open(t)
	if _, err = target.importSQLiteSnapshot(context.Background(), source, path, true); err == nil || !strings.Contains(err.Error(), "stop SQLite writer") {
		t.Fatalf("live=%v", err)
	}
	failModel(t, src, "bad")
	if err = src.Record(retryEvent("bad")); err != nil {
		t.Fatal(err)
	}
	if err = src.Close(); err == nil {
		t.Fatal("expected pending")
	}
	if _, err = target.importSQLiteSnapshot(context.Background(), source, path, true); err == nil || !strings.Contains(err.Error(), "drain SQLite outbox") {
		t.Fatalf("pending=%v", err)
	}
}
