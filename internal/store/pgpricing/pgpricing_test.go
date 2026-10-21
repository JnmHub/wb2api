package pgpricing

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"wb2api/internal/platform/pg"
	"wb2api/internal/pricing"
)

func TestPostgresPricingPolicies(t *testing.T) {
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
	if _, err := pool.Exec(ctx, `delete from wb_state where key = 'pricing_policies'`); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	seed := `{"policies":{"seed-model":{"mode":"manual","output":3.5,"note":"from file"}}}`
	if err := os.WriteFile(filepath.Join(dir, "pricing_policies.json"), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	src := New(pool.Pool, dir)
	got, err := src.LoadPolicies()
	if err != nil {
		t.Fatal(err)
	}
	if p := got["seed-model"]; p.Mode != "manual" || p.Output == nil || *p.Output != 3.5 {
		t.Fatalf("seeded policies = %+v", got)
	}
	if err := src.SavePolicies(map[string]pricing.Policy{"other": {Mode: "ignore"}}); err != nil {
		t.Fatal(err)
	}
	got, err = src.LoadPolicies()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["other"].Mode != "ignore" {
		t.Fatalf("saved policies = %+v", got)
	}
}
