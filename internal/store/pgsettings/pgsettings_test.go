package pgsettings

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"wb2api/internal/store/settings"
)

func TestSeedDocumentValidation(t *testing.T) {
	dir := t.TempDir()
	if doc, err := seedDocument(dir); err != nil || len(doc) != 0 {
		t.Fatalf("fresh install: %v, %v", doc, err)
	}
	for _, raw := range []string{`null`, `[]`, `{"a":`, `{} {}`, `{} garbage`} {
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := seedDocument(dir); err == nil {
			t.Fatalf("accepted malformed settings %q", raw)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte("\xef\xbb\xbf{\"unknown\":1}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if doc, err := seedDocument(dir); err != nil || doc["unknown"] == nil {
		t.Fatalf("valid seed: %v, %v", doc, err)
	}
}

// Two real pools model independent replicas; the dedicated schema isolates this
// singleton settings row from other integration tests sharing the same DB.
func TestPostgresIndependentStoresPreserveConcurrentFields(t *testing.T) {
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
	schema := fmt.Sprintf("pgsettings_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "create schema "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(context.Background(), "drop schema "+quoted+" cascade"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()
	openPool := func() *pgxpool.Pool {
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		cfg.ConnConfig.RuntimeParams["search_path"] = schema
		p, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p1, p2 := openPool(), openPool()
	defer p1.Close()
	defer p2.Close()
	if _, err := p1.Exec(ctx, `create table wb_settings(id boolean primary key, data jsonb not null, updated_at timestamptz not null)`); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(ctx, p1, ""); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("runtime missing row: %v", err)
	}
	dir := t.TempDir()
	if _, err := Load(ctx, p1, dir); err != nil {
		t.Fatal(err)
	}
	makeStore := func(p *pgxpool.Pool) *settings.Store {
		return settings.OpenWithUpdater(
			func() (settings.Doc, error) { return Load(ctx, p, "") },
			func(d settings.Doc) error { return Save(ctx, p, d) },
			func(fn func(settings.Doc) error) (settings.Doc, error) { return Update(ctx, p, fn) },
		)
	}
	a, b := makeStore(p1), makeStore(p2)
	_ = a.Get()
	_ = b.Get() // both caches start with the same stale empty document
	entered := make(chan struct{})
	release := make(chan struct{})
	errs := make(chan error, 2)
	var calls atomic.Int32
	go func() {
		errs <- a.Update(func(d settings.Doc) error {
			calls.Add(1)
			d["alpha"] = "one"
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	started := make(chan struct{})
	go func() {
		close(started)
		errs <- b.Update(func(d settings.Doc) error {
			calls.Add(1)
			d["beta"] = "two"
			return nil
		})
	}()
	<-started
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	doc, err := Load(ctx, p1, "")
	if err != nil || doc["alpha"] != "one" || doc["beta"] != "two" || calls.Load() != 2 {
		t.Fatalf("lost fields or replayed callback: doc=%v calls=%d err=%v", doc, calls.Load(), err)
	}
	abort := errors.New("abort update")
	if err := a.Update(func(d settings.Doc) error { d["alpha"] = "bad"; return abort }); !errors.Is(err, abort) {
		t.Fatalf("abort = %v", err)
	}
	// Existing DB state is authoritative even when a legacy seed is malformed.
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte("bad JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err = Load(ctx, p2, dir)
	if err != nil || doc["alpha"] != "one" || doc["beta"] != "two" {
		t.Fatalf("seed/abort overwrote authoritative row: %v, %v", doc, err)
	}
	// API keys share this document: simultaneous distinct creates must survive,
	// and uniqueness checks must be serialized for the same supplied secret.
	for _, store := range []*settings.Store{a, b} {
		go func(s *settings.Store) {
			_, err := s.CreateKey(settings.KeyInput{})
			errs <- err
		}(store)
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	secret := "integration-test-duplicate-secret"
	for _, store := range []*settings.Store{a, b} {
		go func(s *settings.Store) {
			_, err := s.CreateKey(settings.KeyInput{Key: &secret})
			errs <- err
		}(store)
	}
	successes := 0
	for i := 0; i < 2; i++ {
		if err := <-errs; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("duplicate secret accepted %d times", successes)
	}
	doc, err = Load(ctx, p1, "")
	keys, _ := doc["api_keys"].([]any)
	if err != nil || len(keys) != 3 || doc["alpha"] != "one" || doc["beta"] != "two" {
		t.Fatalf("key mutation lost settings/keys: fields=%d keys=%d err=%v", len(doc), len(keys), err)
	}
}

func TestDecodeSettings(t *testing.T) {
	doc, err := decode([]byte(`{"upstream":{"header_timeout_seconds":30},"n":1}`))
	if err != nil {
		t.Fatal(err)
	}
	up, _ := doc["upstream"].(map[string]any)
	if up["header_timeout_seconds"] == nil || doc["n"] == nil {
		t.Fatalf("doc = %v", doc)
	}
}
