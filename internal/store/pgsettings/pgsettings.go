// Package pgsettings stores the panel settings document in PostgreSQL. It reads
// the existing settings.json once when the database is empty so upgrades keep
// the current configuration.
package pgsettings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"wb2api/internal/store/settings"
)

// Load returns the authoritative row. Only startup calls with a nonempty
// seedDir may initialize it; runtime reads never recreate a missing row.
func Load(ctx context.Context, pool *pgxpool.Pool, seedDir string) (settings.Doc, error) {
	if pool == nil {
		return nil, errors.New("postgres not configured")
	}
	var raw []byte
	err := pool.QueryRow(ctx, `select data from wb_settings where id = true`).Scan(&raw)
	if err == nil {
		return decode(raw)
	}
	if !errors.Is(err, pgx.ErrNoRows) || seedDir == "" {
		return nil, err
	}
	doc, err := seedDocument(seedDir)
	if err != nil {
		return nil, err
	}
	raw, err = json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	// Never overwrite another startup's seed or a concurrent committed update.
	if _, err := pool.Exec(ctx, `insert into wb_settings(id, data, updated_at) values(true, $1::jsonb, now()) on conflict (id) do nothing`, string(raw)); err != nil {
		return nil, err
	}
	return Load(ctx, pool, "")
}

func seedDocument(dir string) (settings.Doc, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		return settings.Doc{}, nil
	}
	if err != nil {
		return nil, err
	}
	return decode(raw)
}

// Save replaces the singleton settings row. Call Update for read/modify/write.
func Save(ctx context.Context, pool *pgxpool.Pool, doc settings.Doc) error {
	if pool == nil {
		return errors.New("postgres not configured")
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, settingsWriteLock); err != nil {
		return err
	}
	if err := save(ctx, tx, doc); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Update loads the latest row after taking the shared write lock and invokes fn
// once in that transaction. It never retries a callback, including on commit
// failure, because settings callbacks can generate secrets or have side effects.
func Update(ctx context.Context, pool *pgxpool.Pool, fn func(settings.Doc) error) (settings.Doc, error) {
	if pool == nil {
		return nil, errors.New("postgres not configured")
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, settingsWriteLock); err != nil {
		return nil, err
	}
	var raw []byte
	if err := tx.QueryRow(ctx, `select data from wb_settings where id = true for update`).Scan(&raw); err != nil {
		return nil, err
	}
	doc, err := decode(raw)
	if err != nil {
		return nil, err
	}
	if err := fn(doc); err != nil {
		return nil, err
	}
	if err := save(ctx, tx, doc); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return doc, nil
}

// wb2api-S, deliberately distinct from the schema migration lock (wb2api-s).
const settingsWriteLock int64 = 0x7762326170692d53

func save(ctx context.Context, tx pgx.Tx, doc settings.Doc) error {
	if doc == nil {
		doc = settings.Doc{}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `insert into wb_settings(id, data, updated_at) values(true, $1::jsonb, now())
		on conflict (id) do update set data = excluded.data, updated_at = now()`, string(raw))
	return err
}

func decode(raw []byte) (settings.Doc, error) {
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, settings.ErrNotObject
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("settings document contains trailing JSON")
		}
		return nil, err
	}
	return settings.Doc(m), nil
}
