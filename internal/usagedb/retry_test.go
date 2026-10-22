package usagedb

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func retryEvent(model string) Event {
	return Event{At: time.Now(), Model: model, Account: "a", Realm: "intl", Outcome: "completed", Total: 7, Credit: 0.25}
}
func failModel(t *testing.T, d *DB, model string) {
	t.Helper()
	_, err := d.sql.Exec("CREATE TRIGGER reject_retry BEFORE INSERT ON requests WHEN NEW.model = '" + model + "' BEGIN SELECT RAISE(ABORT, 'injected usage failure'); END")
	if err != nil {
		t.Fatal(err)
	}
}
func checkRetryTotals(t *testing.T, d *DB, n int64) {
	t.Helper()
	for _, table := range []string{"requests", "usage_facts", "daily", "hourly"} {
		var req, total int64
		var cr float64
		count := "COUNT(*)"
		if table == "daily" || table == "hourly" {
			count = "SUM(requests)"
		}
		if err := d.sql.QueryRow("SELECT COALESCE("+count+",0),COALESCE(SUM(total),0),COALESCE(SUM(credit),0) FROM "+table).Scan(&req, &total, &cr); err != nil {
			t.Fatal(err)
		}
		if req != n || total != n*7 || cr != float64(n)*0.25 {
			t.Fatalf("%s=%d/%d/%v want %d", table, req, total, cr, n)
		}
	}
}
func TestUsageRetryRetainsFailedBatchUntilRecovery(t *testing.T) {
	d := open(t)
	failModel(t, d, "bad")
	if err := d.Record(retryEvent("bad")); err != nil {
		t.Fatal(err)
	}
	if err := d.Flush(); err == nil || !strings.Contains(err.Error(), "pending retry") {
		t.Fatalf("Flush=%v", err)
	}
	checkRetryTotals(t, d, 0)
	if d.Stats().PendingRetry != 1 {
		t.Fatal(d.Stats())
	}
	if err := d.Ready(); err != nil {
		t.Fatalf("sink outage must still accept locally: %v", err)
	}
	if _, err := d.sql.Exec("DROP TRIGGER reject_retry"); err != nil {
		t.Fatal(err)
	}
	if err := d.Flush(); err != nil {
		t.Fatal(err)
	}
	checkRetryTotals(t, d, 1)
	if err := d.Flush(); err != nil {
		t.Fatal(err)
	}
	checkRetryTotals(t, d, 1)
}
func TestUsageRetryLostCommitReplyReopenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	d, err := Open(path, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Record(retryEvent("m")); err != nil {
		t.Fatal(err)
	}
	// Stop only the dispatcher, retaining a real durable outbox row. Commit to
	// sink without deleting that row, simulating a lost COMMIT reply and crash.
	close(d.stop)
	<-d.done
	u, err := d.loadUnit(context.Background(), 1<<62)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.writeBatch(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	checkRetryTotals(t, d, 1)
	// Acknowledge window: replay can happen any number of times before delete.
	for range 3 {
		if err = d.writeBatch(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	checkRetryTotals(t, d, 1)
	close(d.admissionQ)
	<-d.admissionDone
	d.writerCancel()
	d.outbox.Close()
	unlockOutbox(d.outboxLock)
	d.sql.Close()
	d, err = Open(path, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.Flush(); err != nil {
		t.Fatal(err)
	}
	checkRetryTotals(t, d, 1)
	if d.Health().PendingEvents != 0 {
		t.Fatal(d.Health())
	}
	var receipts int
	if err = d.sql.QueryRow("SELECT COUNT(*) FROM usage_sources").Scan(&receipts); err != nil || receipts != 1 {
		t.Fatalf("receipt=%d err=%v", receipts, err)
	}
	// AUTOINCREMENT must not reuse the now-empty queue's first sequence.
	if err = d.Record(retryEvent("next")); err != nil {
		t.Fatal(err)
	}
	if err = d.Flush(); err != nil {
		t.Fatal(err)
	}
	checkRetryTotals(t, d, 2)
}
func TestUsageRetryCloseFailurePreservesOutboxForRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	d, err := Open(path, 32)
	if err != nil {
		t.Fatal(err)
	}
	failModel(t, d, "bad")
	if err = d.Record(retryEvent("bad")); err != nil {
		t.Fatal(err)
	}
	if err = d.Close(); err == nil {
		t.Fatal("Close must report undelivered durable events")
	}
	if err = d.Close(); err == nil {
		t.Fatal("Close lost prior error")
	}
	if err = d.Record(retryEvent("late")); !errors.Is(err, errNoDB) {
		t.Fatal(err)
	}
	if err = d.Flush(); !errors.Is(err, errNoDB) {
		t.Fatal(err)
	}
	d, err = Open(path, 32)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err = d.sql.Exec("DROP TRIGGER reject_retry"); err != nil {
		t.Fatal(err)
	}
	if err = d.Flush(); err != nil {
		t.Fatal(err)
	}
	checkRetryTotals(t, d, 1)
}
func TestUsageRetryTimeoutRetainsAndRecovers(t *testing.T) {
	d, err := OpenWithOptions(filepath.Join(t.TempDir(), "usage.db"), Options{QueueSize: 16, WriteTimeout: 25 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.sql.SetMaxOpenConns(1)
	conn, err := d.sql.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Record(retryEvent("m")); err != nil {
		t.Fatal(err)
	}
	if err = d.Flush(); err == nil {
		t.Fatal("expected timeout")
	}
	conn.Close()
	if err = d.Flush(); err != nil {
		t.Fatal(err)
	}
	checkRetryTotals(t, d, 1)
}
func TestUsageRetryOverflowNeverDropsAccounting(t *testing.T) {
	d, err := OpenWithOptions(filepath.Join(t.TempDir(), "usage.db"), Options{QueueSize: 8, Overflow: OverflowSample, SampleEvery: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	failModel(t, d, "bad")
	if err = d.Record(retryEvent("bad")); err != nil {
		t.Fatal(err)
	}
	_ = d.Flush()
	for range 100 {
		if err = d.Record(retryEvent("good")); err != nil {
			t.Fatal(err)
		}
	}
	if d.Stats().Dropped == 0 || d.Health().PendingEvents != 101 {
		t.Fatalf("stats=%+v health=%+v", d.Stats(), d.Health())
	}
	d.sql.Exec("DROP TRIGGER reject_retry")
	if err = d.Flush(); err != nil {
		t.Fatal(err)
	}
	a, err := d.Totals(Filter{})
	if err != nil || a.Requests != 101 || a.Total != 707 {
		t.Fatalf("aggregate=%+v err=%v", a, err)
	}
	exact, err := d.Totals(Filter{Since: time.Unix(1, 0)})
	if err != nil || exact.Total != 707 {
		t.Fatalf("exact=%+v %v", exact, err)
	}
	p, err := d.PairPointsContext(context.Background(), time.Unix(1, 0), time.Now().Add(time.Hour))
	if err != nil || len(p) != 101 {
		t.Fatalf("points=%d %v", len(p), err)
	}
	if err = d.RebuildRollups(context.Background()); err != nil {
		t.Fatal(err)
	}
	a, err = d.Totals(Filter{})
	if err != nil || a.Total != 707 {
		t.Fatalf("rebuilt=%+v %v", a, err)
	}
}
func TestUsageRetryOutboxBudgetAndOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	d, err := OpenWithOptions(path, Options{OutboxMaxBytes: 2048, MaxEventBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if other, err := Open(path, 32); err == nil {
		other.Close()
		t.Fatal("two writers acquired same durable source")
	}
	failModel(t, d, "bad")
	accepted := 0
	for range 100 {
		err = d.Record(retryEvent("bad"))
		if errors.Is(err, ErrOutboxFull) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		accepted++
	}
	if accepted == 0 || accepted == 100 || d.Health().PendingBytes > 2048 {
		t.Fatal(d.Health())
	}
	if !errors.Is(d.Ready(), ErrOutboxFull) {
		t.Fatalf("Ready=%v", d.Ready())
	}
	d.sql.Exec("DROP TRIGGER reject_retry")
	if err = d.Flush(); err != nil {
		t.Fatal(err)
	}
	checkRetryTotals(t, d, int64(accepted))
	if err = d.Ready(); err != nil {
		t.Fatal(err)
	}
}
func TestUsageRetryLocalFailureIsSticky(t *testing.T) {
	d := open(t)
	if _, err := d.outbox.Exec("CREATE TRIGGER fail_outbox BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT,'injected disk failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := d.Record(retryEvent("m")); err == nil {
		t.Fatal("expected durability error")
	}
	if err := d.Ready(); err == nil {
		t.Fatal("must reject new paid work")
	}
	d.outbox.Exec("DROP TRIGGER fail_outbox")
	if err := d.Record(retryEvent("m")); err == nil {
		t.Fatal("failure must remain sticky until reopen")
	}
}
func TestUsageRetryConcurrentRecordClose(t *testing.T) {
	d := open(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_ = d.Record(retryEvent("m"))
			}
		}()
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if n := d.Written() + d.Dropped(); n != 800 {
		t.Fatalf("unaccounted admission: %d", n)
	}
}
