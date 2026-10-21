package pgsessions

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"wb2api/internal/panelauth"
	"wb2api/internal/platform/pg"
)

func TestPostgresPanelSessionStore(t *testing.T) {
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
	if _, err := pool.Exec(ctx, `truncate wb_panel_sessions, wb_panel_meta`); err != nil {
		t.Fatal(err)
	}
	be := New(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	dir := t.TempDir()
	legacy := map[string]any{"version": 1, "sessions": []panelauth.Session{{
		ID: "fedcba9876543210", CSRF: "legacy", Created: now, LastSeen: now,
		Expires: now.Add(panelauth.RememberTTL), Remember: true,
	}}}
	raw, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(dir, panelauth.SessionFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := be.MigrateFile(dir); err != nil || n != 1 {
		t.Fatalf("migrate = %d, %v; want 1", n, err)
	}
	if n, err := be.MigrateFile(dir); err != nil || n != 0 {
		t.Fatalf("second migrate = %d, %v; want 0", n, err)
	}
	// Once the marker is set, an empty table must not resurrect the file.
	if _, err := pool.Exec(ctx, `truncate wb_panel_sessions`); err != nil {
		t.Fatal(err)
	}
	if n, err := be.MigrateFile(dir); err != nil || n != 0 {
		t.Fatalf("migrate after revoke = %d, %v; want 0", n, err)
	}
	s, err := panelauth.OpenShared(be)
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return now }
	token, created, err := s.Create(true, "pg-test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Lookup(token); !ok {
		t.Fatal("session not found in PostgreSQL")
	}
	if got, ok, err := be.Get(created.ID); err != nil || !ok || got.UserAgent != "pg-test" {
		t.Fatalf("get = %+v ok=%v err=%v", got, ok, err)
	}
	now = now.Add(2 * time.Minute)
	if _, touched, ok := s.LookupTouch(token); !ok || !touched {
		t.Fatalf("touch ok=%v touched=%v", ok, touched)
	}
	if _, _, err := s.Create(false, "second", ""); err != nil {
		t.Fatal(err)
	}
	if list, err := be.List(); err != nil || len(list) != 2 {
		t.Fatalf("list = %d, %v", len(list), err)
	}
	if n := s.RevokeAllExcept(token); n != 1 {
		t.Fatalf("revoke all except = %d, want 1", n)
	}
	if list := s.List(); len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("left after revoke = %+v", list)
	}
	s.Revoke(token)
	if list, err := be.List(); err != nil || len(list) != 0 {
		t.Fatalf("after revoke = %d, %v", len(list), err)
	}
}
