package pgsessions

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"wb2api/internal/panelauth"
	"wb2api/internal/platform/pg"
)

type importLockTracer struct{ started chan struct{} }

func (tr *importLockTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "pg_advisory_xact_lock") {
		select {
		case tr.started <- struct{}{}:
		default:
		}
	}
	return ctx
}

func (*importLockTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestMigrationRechecksMarkerAfterConcurrentImporter(t *testing.T) {
	dsn := os.Getenv("WB_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("WB_TEST_POSTGRES_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("pgsessions_import_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "create schema "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(context.Background(), "drop schema "+quoted+" cascade"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	tracer := &importLockTracer{started: make(chan struct{}, 1)}
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `
		create table wb_panel_meta(key text primary key,value text not null);
		create table wb_panel_sessions(id text primary key,csrf text not null,
		created timestamptz not null,last_seen timestamptz not null,expires timestamptz not null,
		remember boolean not null,user_agent text not null,ip text not null)`); err != nil {
		t.Fatal(err)
	}
	// Importer A owns the first-import lock. Importer B must not read the
	// absent marker until A commits it; otherwise B can reinsert a session
	// that A imported and a browser subsequently revoked.
	first, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(context.Background())
	if _, err := first.Exec(ctx, `select pg_advisory_xact_lock($1)`, fileImportLock); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	now := time.Now().UTC()
	ss := panelauth.Session{ID: "revoked-import-session", CSRF: "csrf", Created: now, LastSeen: now, Expires: now.Add(time.Hour)}
	raw, err := json.Marshal(map[string]any{"version": 1, "sessions": []panelauth.Session{ss}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, panelauth.SessionFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	store := New(&pg.Pool{Pool: pool})
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := store.MigrateFile(dir)
		done <- result{n: n, err: err}
	}()
	select {
	case <-tracer.started:
	case got := <-done:
		t.Fatalf("migration bypassed first-import lock: n=%d err=%v", got.n, got.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Commit the marker with no live sessions (the previously imported
	// session was revoked). B must recheck it after acquiring the lock.
	if _, err := first.Exec(ctx, "insert into "+quoted+`.wb_panel_meta(key,value) values('panel_file_sessions_imported','1')`); err != nil {
		t.Fatal(err)
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.n != 0 {
			t.Fatalf("delayed migration ignored committed marker: n=%d err=%v", got.n, got.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, ok, err := store.Get(ss.ID); err != nil || ok {
		t.Fatalf("revoked imported session resurrected: ok=%v err=%v", ok, err)
	}
}
