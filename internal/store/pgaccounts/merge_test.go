package pgaccounts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"wb2api/internal/platform/pg"
	"wb2api/internal/store/accounts"
)

func TestMergePreservesManagementAndRejectsConflict(t *testing.T) {
	base := []byte(`{"uid":"u","enabled":true,"weight":1,"proxySlot":"a","accessToken":"old"}`)
	proposed := []byte(`{"uid":"u","enabled":true,"weight":1,"proxySlot":"a","accessToken":"new"}`)
	current := []byte(`{"uid":"u","enabled":false,"weight":9,"proxySlot":"b","accessToken":"old"}`)
	got, err := merge(base, proposed, current)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range [][]byte{[]byte(`"enabled":false`), []byte(`"weight":9`), []byte(`"proxySlot":"b"`), []byte(`"accessToken":"new"`)} {
		if !bytes.Contains(got, want) {
			t.Fatalf("merge lost %s: %s", want, got)
		}
	}
	if _, err := merge(base, proposed, []byte(`{"uid":"u","accessToken":"rotated"}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestPostgresDeletedSaveAndIntentionalImport(t *testing.T) {
	dsn := os.Getenv("WB_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("WB_TEST_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	db, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	uid := "account-merge-test-" + time.Now().Format("20060102150405.000000000")
	original := &accounts.Record{UID: uid, AccessToken: "old", Weight: 1, Enabled: true}
	if err := Save(ctx, db.Pool, original); err != nil {
		t.Fatal(err)
	}
	defer Delete(ctx, db.Pool, original)
	raw, _ := original.Marshal()
	stale, err := decode(uid, raw, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := Delete(ctx, db.Pool, original); err != nil {
		t.Fatal(err)
	}
	stale.AccessToken = "new"
	if err := Save(ctx, db.Pool, stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("deleted stale save: %v", err)
	}
	var exists bool
	if err := db.Pool.QueryRow(ctx, `select exists(select 1 from wb_accounts where uid=$1)`, uid).Scan(&exists); err != nil || exists {
		t.Fatalf("resurrected=%v err=%v", exists, err)
	}
	imported := &accounts.Record{UID: uid, AccessToken: "intentional"}
	if err := Save(ctx, db.Pool, imported); err != nil {
		t.Fatal(err)
	}
	collision := &accounts.Record{UID: uid, AccessToken: "replacement"}
	if err := Save(ctx, db.Pool, collision); err != nil {
		t.Fatal(err)
	}
	raw, _ = collision.Marshal()
	management, _ := decode(uid, raw, time.Now())
	refresh, _ := decode(uid, raw, time.Now())
	management.Enabled = false
	management.Weight = 9
	management.ProxySlot = "managed"
	if err := Save(ctx, db.Pool, management); err != nil {
		t.Fatal(err)
	}
	refresh.AccessToken = "refreshed"
	if err := Save(ctx, db.Pool, refresh); err != nil {
		t.Fatal(err)
	}
	var persisted []byte
	if err := db.Pool.QueryRow(ctx, `select data from wb_accounts where uid=$1`, uid).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	got, err := decode(uid, persisted, time.Now())
	if err != nil || got.IsEnabled() || got.Weight != 9 || got.ProxySlot != "managed" || got.AccessToken != "refreshed" {
		t.Fatalf("stale refresh lost management fields: %v", err)
	}
	refresh.AccessToken = "conflicting"
	if err := Save(ctx, db.Pool, refresh); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting stale token save: %v", err)
	}
}

func TestPostgresSeedDoesNotResurrectDeletedAccounts(t *testing.T) {
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
	schema := fmt.Sprintf("pgaccounts_test_%d", time.Now().UnixNano())
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
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.Exec(ctx, `create table wb_accounts(uid text primary key,data jsonb not null,updated_at timestamptz not null default now()); create table wb_panel_meta(key text primary key,value text not null)`); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	notDir := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(notDir, []byte("seed directory unavailable"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path    string
		wantErr bool
	}{
		{path: filepath.Join(dir, "missing"), wantErr: false},
		{path: notDir, wantErr: true},
	} {
		if recs, bad, err := Load(ctx, p, tc.path); (err != nil) != tc.wantErr || len(recs) != 0 || len(bad) != 0 {
			t.Fatalf("seed with unavailable directory %q: count=%d bad=%d err=%v; wantErr=%v", tc.path, len(recs), len(bad), err, tc.wantErr)
		}
		var marked bool
		if err := p.QueryRow(ctx, `select exists(select 1 from wb_panel_meta where key='accounts_seed_v1')`).Scan(&marked); err != nil || marked {
			t.Fatalf("failed directory read consumed seed marker: marked=%v err=%v", marked, err)
		}
		var count int
		if err := p.QueryRow(ctx, `select count(*) from wb_accounts`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed seed changed accounts: count=%d err=%v", count, err)
		}
	}
	// Neither a missing fresh-install directory nor an unreadable path consumes
	// the marker. Once corrected, the complete import can proceed.
	if err := os.WriteFile(filepath.Join(dir, "seed.json"), []byte(`{"uid":"seed","accessToken":"fake","realm":"cn","enabled":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	recs, bad, err := Load(ctx, p, dir)
	if err != nil || len(bad) != 0 || len(recs) != 1 {
		t.Fatalf("seed: count=%d bad=%d err=%v", len(recs), len(bad), err)
	}
	if err := Delete(ctx, p, recs[0]); err != nil {
		t.Fatal(err)
	}
	recs, bad, err = Load(ctx, p, dir)
	if err != nil || len(bad) != 0 || len(recs) != 0 {
		t.Fatalf("resurrected seed: count=%d bad=%d err=%v", len(recs), len(bad), err)
	}
}
