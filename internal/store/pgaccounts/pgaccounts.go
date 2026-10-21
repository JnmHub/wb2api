// Package pgaccounts stores account records in PostgreSQL. The file store stays
// the default for standalone deployments; the database store is selected with
// WB_POSTGRES_DSN and is safe for more than one gateway process.
package pgaccounts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"wb2api/internal/store/accounts"
)

// Load returns every account from PostgreSQL. If the table is empty and
// seedDir is non-empty, it imports the legacy account files first.
func Load(ctx context.Context, pool *pgxpool.Pool, seedDir string) ([]*accounts.Record, []accounts.LoadError, error) {
	if pool == nil {
		return nil, nil, errors.New("postgres not configured")
	}
	rows, err := pool.Query(ctx, `select uid, data from wb_accounts order by uid`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	now := time.Now()
	out := make([]*accounts.Record, 0)
	var errs []accounts.LoadError
	for rows.Next() {
		var uid string
		var raw []byte
		if err := rows.Scan(&uid, &raw); err != nil {
			return nil, nil, err
		}
		rec, err := decode(uid, raw, now)
		if err != nil {
			errs = append(errs, accounts.LoadError{File: "postgres:" + uid, Err: err})
			continue
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(out) > 0 || seedDir == "" {
		return out, errs, nil
	}

	// Empty database: import the existing files once. This makes an upgrade
	// non-destructive and lets a compose volume with accounts/ work unchanged.
	seed, seedErrs := accounts.Load(seedDir, now)
	errs = append(errs, seedErrs...)
	for _, rec := range seed {
		if err := Save(ctx, pool, rec); err != nil {
			return nil, errs, fmt.Errorf("seed %s: %w", rec.UID, err)
		}
		out = append(out, rec)
	}
	return out, errs, nil
}

func decode(uid string, raw []byte, now time.Time) (*accounts.Record, error) {
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("not a JSON object")
	}
	if v, _ := m["uid"].(string); v == "" {
		m["uid"] = uid
	}
	return accounts.FromMap(m, "", now), nil
}

// Save upserts one account.
func Save(ctx context.Context, pool *pgxpool.Pool, rec *accounts.Record) error {
	if pool == nil || rec == nil || rec.UID == "" {
		return errors.New("invalid account")
	}
	raw, err := rec.Marshal()
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `
		insert into wb_accounts(uid, data, updated_at) values($1, $2::jsonb, now())
		on conflict (uid) do update set data = excluded.data, updated_at = now()`, rec.UID, string(raw))
	return err
}

// Delete removes one account by uid.
func Delete(ctx context.Context, pool *pgxpool.Pool, rec *accounts.Record) error {
	if pool == nil || rec == nil || rec.UID == "" {
		return nil
	}
	_, err := pool.Exec(ctx, `delete from wb_accounts where uid = $1`, rec.UID)
	return err
}

// Count is used by health/status endpoints.
func Count(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	if pool == nil {
		return 0, errors.New("postgres not configured")
	}
	var n int64
	err := pool.QueryRow(ctx, `select count(*) from wb_accounts`).Scan(&n)
	return n, err
}
