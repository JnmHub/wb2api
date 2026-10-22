package pgaccounts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"wb2api/internal/platform/pg"
	"wb2api/internal/store/accounts"
)

func syncTestDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("WB_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("WB_TEST_POSTGRES_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := fmt.Sprintf("account_sync_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "create schema "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := admin.Exec(cleanup, "drop schema "+quoted+" cascade"); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	})
	// ConnString() returns the original DSN, not mutated RuntimeParams. Put
	// search_path into the actual DSN passed through pg.Open so this helper
	// never touches public tables, for either URI or keyword/value DSNs.
	isolatedDSN := dsn + " search_path=" + name
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		separator := "?"
		if strings.Contains(dsn, "?") {
			separator = "&"
		}
		isolatedDSN = dsn + separator + "search_path=" + name
	}
	db, err := pg.Open(ctx, isolatedDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	var actualSchema string
	if err := db.QueryRow(ctx, `select current_schema()`).Scan(&actualSchema); err != nil || actualSchema != name {
		t.Fatalf("isolated test schema mismatch: got %q want %q err=%v", actualSchema, name, err)
	}
	return ctx, db.Pool
}

func TestPostgresIncrementalDirectSQLAndTombstones(t *testing.T) {
	ctx, db := syncTestDB(t)
	recs, bad, cursor, err := LoadSnapshot(ctx, db, "")
	if err != nil || len(recs) != 0 || len(bad) != 0 || cursor != 0 {
		t.Fatalf("empty snapshot: %v %v %d %v", recs, bad, cursor, err)
	}
	if _, err := db.Exec(ctx, `insert into wb_accounts(uid,data) values('u','{"weight":1,"accessToken":"t"}'),('v','{}')`); err != nil {
		t.Fatal(err)
	}
	recs, deleted, bad, next, err := Changes(ctx, db, cursor)
	if err != nil || len(recs) != 2 || len(deleted) != 0 || len(bad) != 0 || next <= cursor {
		t.Fatalf("direct inserts: n=%d deleted=%v bad=%v next=%d err=%v", len(recs), deleted, bad, next, err)
	}
	cursor = next
	if _, err := db.Exec(ctx, `update wb_accounts set updated_at=now(),data=data where uid='u'`); err != nil {
		t.Fatal(err)
	}
	if recs, deleted, bad, next, err = Changes(ctx, db, cursor); err != nil || len(recs)+len(deleted)+len(bad) != 0 || next != cursor {
		t.Fatalf("no-op write produced changes: %d %v %v %d %v", len(recs), deleted, bad, next, err)
	}
	if _, err := db.Exec(ctx, `update wb_accounts set data=jsonb_set(data,'{weight}','9') where uid='u'; delete from wb_accounts where uid='v'`); err != nil {
		t.Fatal(err)
	}
	recs, deleted, bad, next, err = Changes(ctx, db, cursor)
	if err != nil || len(recs) != 1 || recs[0].UID != "u" || recs[0].Weight != 9 || len(deleted) != 1 || deleted[0] != "v" || len(bad) != 0 {
		t.Fatalf("update/delete: n=%d deleted=%v bad=%v err=%v", len(recs), deleted, bad, err)
	}
	cursor = next
	if _, err := db.Exec(ctx, `delete from wb_accounts where uid='u'; insert into wb_accounts(uid,data) values('u','{"weight":7}')`); err != nil {
		t.Fatal(err)
	}
	recs, deleted, _, cursor, err = Changes(ctx, db, cursor)
	if err != nil || len(recs) != 1 || recs[0].Weight != 7 || len(deleted) != 0 {
		t.Fatalf("delete/reinsert compaction: n=%d deleted=%v err=%v", len(recs), deleted, err)
	}
	if _, err := db.Exec(ctx, `truncate wb_accounts`); err != nil {
		t.Fatal(err)
	}
	recs, deleted, _, cursor, err = Changes(ctx, db, cursor)
	if err != nil || len(recs) != 0 || len(deleted) != 1 || deleted[0] != "u" {
		t.Fatalf("truncate tombstone: n=%d deleted=%v err=%v", len(recs), deleted, err)
	}
	if _, _, _, _, err := Changes(ctx, db, cursor+1); err == nil {
		t.Fatal("future cursor accepted")
	}
}

func TestPostgresIncrementalLateCommitAndRollback(t *testing.T) {
	ctx, db := syncTestDB(t)
	if _, err := db.Exec(ctx, `insert into wb_accounts(uid,data) values('u','{"weight":1}'),('v','{"weight":1}')`); err != nil {
		t.Fatal(err)
	}
	a, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Rollback(ctx)
	if _, err := a.Exec(ctx, `update wb_accounts set data='{"weight":2}' where uid='u'`); err != nil {
		t.Fatal(err)
	}
	// Snapshot starts after A has allocated its version but before A commits.
	// It must see the old clock AND old row, then Changes must see A later.
	recs, _, cursor, err := LoadSnapshot(ctx, db, "")
	if err != nil || len(recs) != 2 || recs[0].Weight != 1 {
		t.Fatalf("snapshot of uncommitted update: %v %v", recs, err)
	}
	b, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Rollback(ctx)
	started, finished := make(chan struct{}), make(chan error, 1)
	go func() {
		close(started)
		_, err := b.Exec(ctx, `delete from wb_accounts where uid='v'`)
		if err == nil {
			err = b.Commit(ctx)
		}
		finished <- err
	}()
	<-started
	select {
	case err := <-finished:
		t.Fatalf("later writer bypassed uncommitted clock holder: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if changed, removed, _, next, err := Changes(ctx, db, cursor); err != nil || len(changed)+len(removed) != 0 || next != cursor {
		t.Fatalf("uncommitted changes visible: %v %v %d %v", changed, removed, next, err)
	}
	if err := a.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	changed, removed, _, next, err := Changes(ctx, db, cursor)
	if err != nil || len(changed) != 1 || changed[0].Weight != 2 || len(removed) != 1 || removed[0] != "v" || next <= cursor {
		t.Fatalf("late commits skipped: n=%d deleted=%v cursor=%d err=%v", len(changed), removed, next, err)
	}
	rollback, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rollback.Exec(ctx, `delete from wb_accounts where uid='u'`); err != nil {
		t.Fatal(err)
	}
	if err := rollback.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if changed, removed, _, end, err := Changes(ctx, db, next); err != nil || len(changed)+len(removed) != 0 || end != next {
		t.Fatalf("rollback leaked cursor or tombstone: n=%d deleted=%v end=%d err=%v", len(changed), removed, end, err)
	}
}

func TestPostgresLoadOneMergedBaselineAndMalformedFailClosed(t *testing.T) {
	ctx, db := syncTestDB(t)
	original := &accounts.Record{UID: "u", AccessToken: "old", Enabled: true, Weight: 1}
	if err := Save(ctx, db, original); err != nil {
		t.Fatal(err)
	}
	stale, err := LoadOne(ctx, db, "u")
	if err != nil {
		t.Fatal(err)
	}
	baseline := append([]byte(nil), stale.SharedBaseline...)
	if _, err := db.Exec(ctx, `update wb_accounts set data=jsonb_set(data,'{weight}','9') where uid='u'`); err != nil {
		t.Fatal(err)
	}
	stale.AccessToken = "refreshed"
	if err := Save(ctx, db, stale); err != nil {
		t.Fatal(err)
	}
	latest, err := LoadOne(ctx, db, "u")
	if err != nil || latest.Weight != 9 || latest.AccessToken != "refreshed" || !bytes.Equal(stale.SharedBaseline, baseline) {
		t.Fatalf("merged reload or immutable baseline failed: %v", err)
	}
	latest.Note = "next edit"
	if err := Save(ctx, db, latest); err != nil {
		t.Fatalf("LoadOne baseline not usable: %v", err)
	}
	_, _, cursor, err := LoadSnapshot(ctx, db, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `update wb_accounts set data='null' where uid='u'`); err != nil {
		t.Fatal(err)
	}
	recs, deleted, bad, _, err := Changes(ctx, db, cursor)
	if err != nil || len(recs) != 0 || len(deleted) != 1 || deleted[0] != "u" || len(bad) != 1 {
		t.Fatalf("malformed record not removed: %d %v %v %v", len(recs), deleted, bad, err)
	}
	if err := Delete(ctx, db, original); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOne(ctx, db, "u"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("deleted LoadOne: %v", err)
	}
}

func TestPostgresSnapshotWatermarkCoherentDuringWrites(t *testing.T) {
	ctx, db := syncTestDB(t)
	if _, err := db.Exec(ctx, `insert into wb_accounts(uid,data) values('u','{"weight":1}')`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for i := 2; i <= 40; i++ {
			if _, err := db.Exec(ctx, `update wb_accounts set data=jsonb_build_object('weight',$1::int) where uid='u'`, i); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for i := 0; i < 40; i++ {
		recs, _, cursor, err := LoadSnapshot(ctx, db, "")
		if err != nil || len(recs) != 1 || int64(recs[0].Weight) != cursor {
			t.Errorf("incoherent snapshot: records=%v cursor=%d err=%v", recs, cursor, err)
			break
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
