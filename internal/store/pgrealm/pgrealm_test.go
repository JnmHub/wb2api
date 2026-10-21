package pgrealm

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"wb2api/internal/platform/pg"
)

func TestPostgresActiveRealm(t *testing.T) {
	dsn := os.Getenv("WB_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("WB_TEST_POSTGRES_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `delete from wb_state where key = 'active_realm'`); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "active_realm.json"), []byte(`{"realm":"cn"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(ctx, pool.Pool, dir, "intl"); err != nil || got != "cn" {
		t.Fatalf("seed load = %q, %v; want cn", got, err)
	}
	if got, err := Get(ctx, pool.Pool); err != nil || got != "cn" {
		t.Fatalf("get after seed = %q, %v; want cn", got, err)
	}
	if err := Save(ctx, pool.Pool, "intl"); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(ctx, pool.Pool, dir, "intl"); err != nil || got != "intl" {
		t.Fatalf("load after save = %q, %v; want intl", got, err)
	}
	if err := Save(ctx, pool.Pool, "mars"); err == nil {
		t.Fatal("invalid realm must be rejected")
	}
	if _, err := pool.Exec(ctx, `delete from wb_state where key = 'active_realm'`); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(ctx, pool.Pool, "", "intl"); err != nil || got != "intl" {
		t.Fatalf("default load = %q, %v; want intl", got, err)
	}
}
