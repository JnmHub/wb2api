package pgquota

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"wb2api/internal/platform/pg"
	"wb2api/internal/quota"
)

func TestPostgresQuotaWindows(t *testing.T) {
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
	if _, err := pool.Exec(ctx, `delete from wb_state where key = 'quota_windows'`); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	seed := `{"windows":[{"uid":"u1","model":"hy3","reset_at":4102444800}]}`
	if err := os.WriteFile(filepath.Join(dir, "model_windows.json"), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	src := New(pool.Pool, dir)
	got, err := src.LoadWindows()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].UID != "u1" || got[0].Model != "hy3" {
		t.Fatalf("seeded windows = %+v", got)
	}
	want := []quota.Window{{UID: "u2", Model: "m2", ResetAt: 4102444801}}
	if err := src.SaveWindows(want); err != nil {
		t.Fatal(err)
	}
	got, err = src.LoadWindows()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].UID != "u2" || got[0].ResetAt != 4102444801 {
		t.Fatalf("saved windows = %+v", got)
	}
}
