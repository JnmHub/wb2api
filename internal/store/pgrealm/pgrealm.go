// Package pgrealm stores the panel's active realm in PostgreSQL. The value
// lives in the generic wb_state table so later shared runtime state (quota,
// pricing, leader election) can use the same row format.
package pgrealm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const stateKey = "active_realm"

// Load returns the shared active realm. When the row does not exist yet it is
// seeded from the legacy accounts/active_realm.json (then def) and persisted.
func Load(ctx context.Context, pool *pgxpool.Pool, seedDir, def string) (string, error) {
	if pool == nil {
		return "", errors.New("postgres not configured")
	}
	realm, err := Get(ctx, pool)
	if err == nil {
		return realm, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	realm = def
	if seedDir != "" {
		if b, err := os.ReadFile(filepath.Join(seedDir, "active_realm.json")); err == nil {
			var d struct {
				Realm string `json:"realm"`
			}
			if json.Unmarshal(b, &d) == nil && valid(d.Realm) {
				realm = d.Realm
			}
		}
	}
	if !valid(realm) {
		realm = "intl"
	}
	if err := Save(ctx, pool, realm); err != nil {
		return "", err
	}
	return realm, nil
}

// Get reads the shared realm; pgx.ErrNoRows means it has not been seeded.
func Get(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	if pool == nil {
		return "", errors.New("postgres not configured")
	}
	var realm string
	err := pool.QueryRow(ctx, `select data->>'realm' from wb_state where key = $1`, stateKey).Scan(&realm)
	if err != nil {
		return "", err
	}
	if !valid(realm) {
		return "", errors.New("invalid active realm in wb_state")
	}
	return realm, nil
}

// Save replaces the shared realm.
func Save(ctx context.Context, pool *pgxpool.Pool, realm string) error {
	if pool == nil {
		return errors.New("postgres not configured")
	}
	if !valid(realm) {
		return errors.New("invalid active realm")
	}
	_, err := pool.Exec(ctx, `
		insert into wb_state(key, data, updated_at) values($1, jsonb_build_object('realm', $2::text), now())
		on conflict (key) do update set data = excluded.data, updated_at = now()`, stateKey, realm)
	return err
}

func valid(realm string) bool { return realm == "intl" || realm == "cn" }
