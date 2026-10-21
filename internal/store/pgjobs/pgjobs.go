// Package pgjobs persists the panel's background job snapshots so every
// replica shows the same history. Running work stays on its owner; the other
// replicas mirror snapshots and route cancellation through CancelRequested.
package pgjobs

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"wb2api/internal/jobs"
)

const keepRows = 500

// Store is a jobs.Store backed by the wb_jobs table.
type Store struct {
	pool  *pgxpool.Pool
	saves atomic.Uint64
}

// New returns a shared job store.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Save upserts one snapshot. A cancel request written by another replica is
// preserved: the owner may save an older snapshot after the request but must
// still observe the flag on its next Sync.
func (s *Store) Save(ctx context.Context, j jobs.Job) error {
	if s == nil || s.pool == nil {
		return errors.New("postgres not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		insert into wb_jobs(id, data, updated_at)
		values($1, $2::jsonb, now())
		on conflict (id) do update set
			data = case
				when wb_jobs.data->>'cancel_requested' = 'true'
				then excluded.data || '{"cancel_requested":true}'::jsonb
				else excluded.data
			end,
			updated_at = now()`, j.ID, string(raw))
	if err != nil {
		return err
	}
	// Batch jobs are frequent; retain a bounded history instead of growing
	// forever. Pruning every 500 saves keeps the extra statement rare.
	if s.saves.Add(1)%500 == 0 {
		_, _ = s.pool.Exec(ctx, `delete from wb_jobs where id in (
			select id from wb_jobs order by updated_at desc offset $1)`, keepRows)
	}
	return nil
}

// Load returns the most recently updated snapshots, newest first.
func (s *Store) Load(ctx context.Context, limit int) ([]jobs.Job, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("postgres not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `select data from wb_jobs order by updated_at desc limit $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]jobs.Job, 0, limit)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var j jobs.Job
		if err := json.Unmarshal(raw, &j); err != nil {
			continue
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// RequestCancel marks a running job cancelled. It reports whether a running
// row matched; the owner observes the flag on its next Sync.
func (s *Store) RequestCancel(ctx context.Context, id string) (bool, error) {
	if s == nil || s.pool == nil {
		return false, errors.New("postgres not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	tag, err := s.pool.Exec(ctx, `
		update wb_jobs
		set data = jsonb_set(data, '{cancel_requested}', 'true'::jsonb),
		    updated_at = now()
		where id = $1 and data->>'state' = 'running'`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
