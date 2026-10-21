// Package pgquota stores model quota windows in the shared wb_state row so a
// 429 discovered by one replica immediately cools the model on the others.
package pgquota

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"wb2api/internal/quota"
)

const (
	stateKey = "quota_windows"
	timeout  = 5 * time.Second
)

// Store is a quota.WindowStore backed by PostgreSQL.
type Store struct {
	pool    *pgxpool.Pool
	seedDir string
}

// New returns a shared window store. seedDir may be empty.
func New(pool *pgxpool.Pool, seedDir string) *Store {
	return &Store{pool: pool, seedDir: seedDir}
}

// LoadWindows reads the shared windows. When the row is absent it imports the
// local model_windows.json, so an upgrade keeps the windows already learned.
func (s *Store) LoadWindows() ([]quota.Window, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("postgres not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var raw []byte
	err := s.pool.QueryRow(ctx, `select data->'windows' from wb_state where key = $1`, stateKey).Scan(&raw)
	if err == nil {
		return decode(raw)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	windows := s.seed()
	if err := s.SaveWindows(windows); err != nil {
		return nil, err
	}
	return windows, nil
}

func (s *Store) seed() []quota.Window {
	if s.seedDir == "" {
		return nil
	}
	path := filepath.Join(s.seedDir, "model_windows.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc struct {
		Windows []quota.Window `json:"windows"`
	}
	if json.Unmarshal(b, &doc) == nil && doc.Windows != nil {
		return doc.Windows
	}
	var bare []quota.Window
	if json.Unmarshal(b, &bare) == nil {
		return bare
	}
	return nil
}

// SaveWindows replaces the shared window set atomically.
func (s *Store) SaveWindows(windows []quota.Window) error {
	if s == nil || s.pool == nil {
		return errors.New("postgres not configured")
	}
	if windows == nil {
		windows = []quota.Window{}
	}
	raw, err := json.Marshal(windows)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err = s.pool.Exec(ctx, `
		insert into wb_state(key, data, updated_at)
		values($1, jsonb_build_object('windows', $2::jsonb), now())
		on conflict (key) do update set data = excluded.data, updated_at = now()`, stateKey, string(raw))
	return err
}

func decode(raw []byte) ([]quota.Window, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var out []quota.Window
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}
