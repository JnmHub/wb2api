package usagedb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDurableLegacyBaselineSurvivesRebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	h, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Exec(schema); err != nil {
		t.Fatal(err)
	}
	old := &DB{sql: h, driver: "sqlite"}
	tx, err := h.Begin()
	if err != nil {
		t.Fatal(err)
	}
	ins, err := old.prepare(tx, insertSQL)
	if err != nil {
		t.Fatal(err)
	}
	daily, err := old.prepare(tx, upsertDailySQL)
	if err != nil {
		t.Fatal(err)
	}
	hourly, err := old.prepare(tx, upsertHourlySQL)
	if err != nil {
		t.Fatal(err)
	}
	st := &stmts{ins: ins, daily: daily, hourly: hourly}
	ev := retryEvent("old")
	if err = st.store(&ev); err != nil {
		t.Fatal(err)
	}
	st.close()
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Historical detail retention must not erase an aggregate-only contribution.
	if _, err = h.Exec("UPDATE daily SET requests=requests+9,total=total+63,credit=credit+2.25; UPDATE hourly SET requests=requests+9,total=total+63,credit=credit+2.25"); err != nil {
		t.Fatal(err)
	}
	h.Close()
	d, err := Open(path, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.Record(retryEvent("new")); err != nil {
		t.Fatal(err)
	}
	if err = d.Flush(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = d.RebuildRollups(context.Background()); err != nil {
			t.Fatal(err)
		}
		a, err := d.Totals(Filter{})
		if err != nil || a.Requests != 11 || a.Total != 77 || a.Credit != 2.75 {
			t.Fatalf("rebuilt legacy=%+v err=%v", a, err)
		}
		var total int64
		if err = d.sql.QueryRow("SELECT SUM(total) FROM hourly").Scan(&total); err != nil || total != 77 {
			t.Fatalf("hourly=%d %v", total, err)
		}
	}
	// Imported post-migration rows must participate in facts and rebuild exactly once.
	// Exercise the shared imported-row accounting transaction.
	tx, err = d.beginImport()
	if err != nil {
		t.Fatal(err)
	}
	ins, err = d.prepare(tx, insertSQL)
	if err != nil {
		t.Fatal(err)
	}
	st, err = d.prepareRollups(tx)
	if err != nil {
		t.Fatal(err)
	}
	st.ins = ins
	ev = retryEvent("imported")
	if err = st.store(&ev); err != nil {
		t.Fatal(err)
	}
	st.close()
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = d.RebuildRollups(context.Background()); err != nil {
		t.Fatal(err)
	}
	a, err := d.Totals(Filter{})
	if err != nil || a.Total != 84 {
		t.Fatalf("imported rebuild=%+v %v", a, err)
	}
}
func TestDurableBadDetailsCannotPoisonAccounting(t *testing.T) {
	d, err := OpenWithOptions(filepath.Join(t.TempDir(), "usage.db"), Options{MaxEventBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, extra := range []json.RawMessage{json.RawMessage("{broken"), json.RawMessage(`{"big":"` + strings.Repeat("x", 8192) + `"}`)} {
		e := retryEvent("m")
		e.Extra = extra
		if err = d.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	if err = d.Flush(); err != nil {
		t.Fatal(err)
	}
	a, err := d.Totals(Filter{})
	if err != nil || a.Requests != 2 || a.Total != 14 || d.Dropped() != 2 {
		t.Fatalf("%+v %v dropped=%d", a, err, d.Dropped())
	}
	e := retryEvent(strings.Repeat("x", 2048))
	if !errors.Is(d.Record(e), ErrEventTooLarge) {
		t.Fatal("accounting dimensions must obey payload bound")
	}
	if err = d.Ready(); err != nil {
		t.Fatal("invalid event must not latch global failure")
	}
}
func TestDurableCancelledCloseRetainsPendingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	d, err := OpenWithOptions(path, Options{WriteTimeout: time.Second, CloseTimeout: 25 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	d.sql.SetMaxOpenConns(1)
	conn, err := d.sql.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Record(retryEvent("m")); err != nil {
		t.Fatal(err)
	}
	// Close waits for outstanding SQL users too, so release that caller-owned
	// connection only once dispatcher cancellation is observed.
	result := make(chan error, 1)
	go func() { result <- d.Close() }()
	select {
	case <-d.done:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not cancel sink connection wait")
	}
	conn.Close()
	if err = <-result; err == nil {
		t.Fatal("expected pending failure")
	}
	d, err = Open(path, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.Flush(); err != nil {
		t.Fatal(err)
	}
	checkRetryTotals(t, d, 1)
}
