// Package pgsettings stores the panel settings document in PostgreSQL. It reads
// the existing settings.json once when the database is empty so upgrades keep
// the current configuration.
package pgsettings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"wb2api/internal/store/settings"
)

// Load returns the settings document. If no row exists, the legacy
// settings.json in seedDir is imported and persisted.
func Load(ctx context.Context, pool *pgxpool.Pool, seedDir string) (settings.Doc, error) {
	if pool == nil {
		return nil, errors.New("postgres not configured")
	}
	var raw []byte
	err := pool.QueryRow(ctx, `select data from wb_settings where id = true`).Scan(&raw)
	if err == nil {
		return decode(raw)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	doc := settings.Doc{}
	if seedDir != "" {
		doc = settings.Open(seedDir).Get()
	}
	if err := Save(ctx, pool, doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// Save replaces the singleton settings row.
func Save(ctx context.Context, pool *pgxpool.Pool, doc settings.Doc) error {
	if pool == nil {
		return errors.New("postgres not configured")
	}
	if doc == nil {
		doc = settings.Doc{}
	}
	raw, err := json.Marshal(map[string]any(doc))
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `
		insert into wb_settings(id, data, updated_at) values(true, $1::jsonb, now())
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
		m = map[string]any{}
	}
	return settings.Doc(m), nil
}
