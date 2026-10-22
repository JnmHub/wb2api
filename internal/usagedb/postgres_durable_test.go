package usagedb

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// Parent provisions WB_TEST_POSTGRES_DSN exclusively for this package; like the
// existing PG tests these clear shared usage tables, never run against live DBs.
func TestPostgresDurableReplayAndSnapshot(t *testing.T) {
	dsn := os.Getenv("WB_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("WB_TEST_POSTGRES_DSN not set")
	}
	outbox := filepath.Join(t.TempDir(), "pg-outbox.db")
	d, err := OpenPostgresWithOptions(dsn, Options{OutboxPath: outbox})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.sql.Exec("TRUNCATE requests,daily,hourly,usage_facts,legacy_daily,legacy_hourly,usage_sources,usage_batches"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.sql.Exec("DELETE FROM meta WHERE k LIKE 'import%'"); err != nil {
		t.Fatal(err)
	}
	// Simulate successful sink commit with lost acknowledgement + process restart.
	if err = d.Record(retryEvent("pg-replay")); err != nil {
		t.Fatal(err)
	}
	close(d.stop)
	<-d.done
	u, err := d.loadUnit(context.Background(), 1<<62)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.writeBatch(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	if err = d.writeBatch(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	close(d.admissionQ)
	<-d.admissionDone
	d.writerCancel()
	d.outbox.Close()
	unlockOutbox(d.outboxLock)
	d.sql.Close()
	d, err = OpenPostgresWithOptions(dsn, Options{OutboxPath: outbox})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.Flush(); err != nil {
		t.Fatal(err)
	}
	checkRetryTotals(t, d, 1)
	// A source with deliberately omitted details must carry every quota value.
	srcPath := filepath.Join(t.TempDir(), "sampled.db")
	src, err := OpenWithOptions(srcPath, Options{QueueSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	failModel(t, src, "bad")
	if err = src.Record(retryEvent("bad")); err != nil {
		t.Fatal(err)
	}
	_ = src.Flush()
	for range 20 {
		if err = src.Record(retryEvent("sampled")); err != nil {
			t.Fatal(err)
		}
	}
	src.sql.Exec("DROP TRIGGER reject_retry")
	if err = src.Close(); err != nil {
		t.Fatal(err)
	}
	source, err := sql.Open("sqlite", "file:"+srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = source.Exec("UPDATE daily SET requests=requests+9,total=total+63,credit=credit+2.25 WHERE model='bad'; UPDATE hourly SET requests=requests+9,total=total+63,credit=credit+2.25 WHERE model='bad'"); err != nil {
		t.Fatal(err)
	}
	source.Close()
	if n, err := d.ImportSQLite(context.Background(), srcPath); err != nil || n != 1 {
		t.Fatalf("import=%d %v", n, err)
	}
	if n, err := d.ImportSQLite(context.Background(), srcPath); err != nil || n != 0 {
		t.Fatalf("reimport=%d %v", n, err)
	}
	for range 2 {
		if err = d.RebuildRollups(context.Background()); err != nil {
			t.Fatal(err)
		}
		a, err := d.Totals(Filter{})
		if err != nil || a.Requests != 31 || a.Total != 217 || a.Credit != 7.75 {
			t.Fatalf("totals=%+v %v", a, err)
		}
	}
	var details, facts int
	if err = d.sql.QueryRow("SELECT COUNT(*) FROM requests").Scan(&details); err != nil {
		t.Fatal(err)
	}
	if err = d.sql.QueryRow("SELECT COUNT(*) FROM usage_facts").Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if details != 2 || facts != 22 {
		t.Fatalf("details=%d facts=%d", details, facts)
	}
}
