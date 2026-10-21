// Package pgquota stores model quota windows and the active-scan schedule in
// the shared wb_state row. A 429 discovered by one replica immediately cools
// the model on the others, and a replica that takes over as leader resumes the
// same scan schedule.
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
	stateKey     = "quota_windows"
	scanStateKey = "quota_scan"
	timeout      = 5 * time.Second
)

// Store is a quota.WindowStore and quota.ScanStore backed by PostgreSQL.
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

// LoadScans reads the shared active-scan schedule. When the row is absent it
// imports quota_scan.json, so the first replica to start after an upgrade keeps
// the retry state already learned from the local file.
func (s *Store) LoadScans() ([]quota.ScanRow, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("postgres not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var raw []byte
	err := s.pool.QueryRow(ctx, `select data->'scans' from wb_state where key = $1`, scanStateKey).Scan(&raw)
	if err == nil {
		return decodeScans(raw)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	rows := s.scanSeed()
	if err := s.SaveScans(rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *Store) scanSeed() []quota.ScanRow {
	if s.seedDir == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(s.seedDir, "quota_scan.json"))
	if err != nil {
		return nil
	}
	var doc struct {
		Scans []quota.ScanRow `json:"scans"`
	}
	if json.Unmarshal(b, &doc) == nil && doc.Scans != nil {
		return doc.Scans
	}
	var bare []quota.ScanRow
	if json.Unmarshal(b, &bare) == nil {
		return bare
	}
	return nil
}

// SaveScans replaces the shared active-scan schedule atomically.
func (s *Store) SaveScans(rows []quota.ScanRow) error {
	if s == nil || s.pool == nil {
		return errors.New("postgres not configured")
	}
	if rows == nil {
		rows = []quota.ScanRow{}
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err = s.pool.Exec(ctx, `
		insert into wb_state(key, data, updated_at)
		values($1, jsonb_build_object('scans', $2::jsonb), now())
		on conflict (key) do update set data = excluded.data, updated_at = now()`, scanStateKey, string(raw))
	return err
}

func decodeScans(raw []byte) ([]quota.ScanRow, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var out []quota.ScanRow
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}
