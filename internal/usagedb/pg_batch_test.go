package usagedb

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRollupSumsRejectOverflow(t *testing.T) {
	s := &rollupSums{}
	if err := s.add(&Event{Total: math.MaxInt64}); err != nil {
		t.Fatal(err)
	}
	if err := s.add(&Event{Total: 1}); err == nil {
		t.Fatal("accepted integer overflow")
	}
	s = &rollupSums{}
	if err := s.add(&Event{Credit: math.MaxFloat64}); err != nil {
		t.Fatal(err)
	}
	if err := s.add(&Event{Credit: math.MaxFloat64}); err == nil {
		t.Fatal("accepted credit overflow")
	}
}

// Real PG path compared to authoritative per-event SQLite behavior across every
// rollup measure, repeated keys, mixed outcomes, sampling and receipt replay.
func TestPostgresBulkMatchesSQLiteAllMeasures(t *testing.T) {
	dsn := os.Getenv("WB_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("WB_TEST_POSTGRES_DSN not set")
	}
	pg, err := OpenPostgresWithOptions(dsn, Options{OutboxPath: filepath.Join(t.TempDir(), "outbox.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	if _, err = pg.sql.Exec("TRUNCATE requests,daily,hourly,usage_facts,legacy_daily,legacy_hourly,usage_sources,usage_batches"); err != nil {
		t.Fatal(err)
	}
	sq, err := Open(filepath.Join(t.TempDir(), "sqlite.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	u := writeUnit{id: "bulk-comparison"}
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.Local)
	for i := range batchMax {
		ttft, gen := int64(i+1), int64(i+10)
		e := Event{At: at.Add(time.Duration(i%5-2) * 24 * time.Hour), Model: []string{"b", "a"}[i%2], Account: []string{"", "a", "z"}[i%3], Realm: []string{"intl", "cn"}[i%2], KeyID: []string{"x", ""}[i%2], Outcome: []string{"completed", "client_aborted", "error", ""}[i%4], ElapsedMs: int64(i), Prompt: int64(i), Completion: 2, Reasoning: 3, Cached: 1, Total: int64(i + 2), Credit: 0.25, Estimated: i%7 == 0, TTFTMs: &ttft, GenMs: &gen, Extra: []byte(`{"detail":true}`)}
		if i%5 == 0 {
			e.TTFTMs = nil
		}
		if i%3 == 0 {
			e.GenMs = nil
		}
		u.events = append(u.events, e)
		u.seqs = append(u.seqs, int64(i+1))
		u.details = append(u.details, i%3 == 0)
	}
	for _, db := range []*DB{sq, pg} {
		for range 2 {
			if err = db.writeBatch(context.Background(), u); err != nil {
				t.Fatal(err)
			}
		}
	}
	read := func(db *DB, table, dims string) [][]string {
		t.Helper()
		q := "SELECT " + dims + "," + measureNames + " FROM " + table + " ORDER BY " + dims
		rows, err := db.sql.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var result [][]string
		width := len(strings.Split(dims, ",")) + len(strings.Split(measureNames, ","))
		for rows.Next() {
			r := make([]string, width)
			dst := make([]any, width)
			for i := range dst {
				dst[i] = &r[i]
			}
			if err = rows.Scan(dst...); err != nil {
				t.Fatal(err)
			}
			result = append(result, r)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	for _, spec := range []struct{ table, dims string }{{"daily", "day,realm,model,account,key_id"}, {"hourly", "hour,realm,model"}} {
		want, got := read(sq, spec.table, spec.dims), read(pg, spec.table, spec.dims)
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("%s mismatch\nSQLite=%v\nPG=%v", spec.table, want, got)
		}
	}
	var facts, details int
	pg.sql.QueryRow("SELECT COUNT(*) FROM usage_facts").Scan(&facts)
	pg.sql.QueryRow("SELECT COUNT(*) FROM requests").Scan(&details)
	if facts != batchMax || details != (batchMax+2)/3 {
		t.Fatalf("facts=%d details=%d", facts, details)
	}
	if err = pg.RebuildRollups(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(read(sq, "daily", "day,realm,model,account,key_id"), read(pg, "daily", "day,realm,model,account,key_id")) {
		t.Fatal("bulk totals changed after rebuild")
	}
}
