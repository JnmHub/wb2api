// Package pgpricing stores per-model pricing overrides in the shared wb_state
// row so every replica prices requests the same way.
package pgpricing

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"wb2api/internal/pricing"
)

const (
	stateKey = "pricing_policies"
	timeout  = 5 * time.Second
)

// Store is a pricing.PolicySource backed by PostgreSQL.
type Store struct {
	pool    *pgxpool.Pool
	seedDir string
}

// New returns a shared policy source. seedDir may be empty.
func New(pool *pgxpool.Pool, seedDir string) *Store {
	return &Store{pool: pool, seedDir: seedDir}
}

// LoadPolicies reads the shared overrides. When the row is absent it imports
// accounts/pricing_policies.json and persists it, so an upgrade is seamless.
func (s *Store) LoadPolicies() (map[string]pricing.Policy, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("postgres not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var raw []byte
	err := s.pool.QueryRow(ctx, `select data->'policies' from wb_state where key = $1`, stateKey).Scan(&raw)
	if err == nil {
		return decode(raw)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	policies := map[string]pricing.Policy{}
	if s.seedDir != "" {
		if b, readErr := os.ReadFile(filepath.Join(s.seedDir, "pricing_policies.json")); readErr == nil {
			var doc struct {
				Policies map[string]pricing.Policy `json:"policies"`
			}
			if json.Unmarshal(b, &doc) == nil && doc.Policies != nil {
				policies = doc.Policies
			}
		}
	}
	if err := s.SavePolicies(policies); err != nil {
		return nil, err
	}
	return policies, nil
}

// SavePolicies replaces the shared overrides atomically.
func (s *Store) SavePolicies(policies map[string]pricing.Policy) error {
	if s == nil || s.pool == nil {
		return errors.New("postgres not configured")
	}
	if policies == nil {
		policies = map[string]pricing.Policy{}
	}
	raw, err := json.Marshal(policies)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err = s.pool.Exec(ctx, `
		insert into wb_state(key, data, updated_at)
		values($1, jsonb_build_object('policies', $2::jsonb), now())
		on conflict (key) do update set data = excluded.data, updated_at = now()`, stateKey, string(raw))
	return err
}

func decode(raw []byte) (map[string]pricing.Policy, error) {
	out := map[string]pricing.Policy{}
	if len(raw) == 0 || string(raw) == "null" {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]pricing.Policy{}
	}
	return out, nil
}
