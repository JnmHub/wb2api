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
	"os"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"wb2api/internal/store/accounts"
)

// Load returns every account from PostgreSQL. A nonempty seedDir enables a
// one-time legacy import; the persisted marker prevents reimport after deletion.
func Load(ctx context.Context, pool *pgxpool.Pool, seedDir string) ([]*accounts.Record, []accounts.LoadError, error) {
	if pool == nil {
		return nil, nil, errors.New("postgres not configured")
	}
	if seedDir != "" {
		if err := seedOnce(ctx, pool, seedDir); err != nil {
			return nil, nil, err
		}
		seedDir = ""
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
	return out, errs, nil
}

func seedOnce(ctx context.Context, pool *pgxpool.Pool, dir string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `select pg_advisory_xact_lock(864235710256)`); err != nil {
		return err
	}
	var done bool
	if err = tx.QueryRow(ctx, `select exists(select 1 from wb_panel_meta where key='accounts_seed_v1')`).Scan(&done); err != nil {
		return err
	}
	if !done {
		var count int64
		if err = tx.QueryRow(ctx, `select count(*) from wb_accounts`).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			recs, bad, loadErr := accounts.LoadStrict(dir, time.Now())
			if errors.Is(loadErr, os.ErrNotExist) {
				// A fresh install need not have an accounts directory yet. Leave
				// the marker unset so a later mounted/restored source can import.
				return tx.Commit(ctx)
			}
			if loadErr != nil {
				return fmt.Errorf("account seed directory: %w", loadErr)
			}
			if len(bad) != 0 {
				return fmt.Errorf("account seed has %d unreadable paths: %w", len(bad), bad[0])
			}
			for _, rec := range recs {
				raw, e := rec.Marshal()
				if e != nil {
					return e
				}
				if _, e = tx.Exec(ctx, `insert into wb_accounts(uid,data) values($1,$2::jsonb) on conflict(uid) do nothing`, rec.UID, string(raw)); e != nil {
					return e
				}
			}
		}
		if _, err = tx.Exec(ctx, `insert into wb_panel_meta(key,value) values('accounts_seed_v1','done') on conflict do nothing`); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
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
	rec := accounts.FromMap(m, "", now)
	baseline, err := rec.Marshal()
	if err != nil {
		return nil, err
	}
	rec.SharedBaseline = baseline
	return rec, nil
}

// ErrConflict means another replica changed a field this save also changed.
var ErrConflict = errors.New("account changed concurrently")

// Save deliberately upserts fresh imports. Records loaded from PostgreSQL carry
// a baseline: their changed fields are merged under a row lock, and a deleted
// account is never recreated by a delayed refresh or management operation.
func Save(ctx context.Context, pool *pgxpool.Pool, rec *accounts.Record) error {
	if pool == nil || rec == nil || rec.UID == "" {
		return errors.New("invalid account")
	}
	raw, err := rec.Marshal()
	if err != nil {
		return err
	}
	if rec.SharedBaseline == nil {
		_, err = pool.Exec(ctx, `
			insert into wb_accounts(uid, data, updated_at) values($1, $2::jsonb, now())
			on conflict (uid) do update set data = excluded.data, updated_at = now()`, rec.UID, string(raw))
		if err == nil {
			rec.SharedBaseline = raw
		}
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var current []byte
	if err := tx.QueryRow(ctx, `select data from wb_accounts where uid = $1 for update`, rec.UID).Scan(&current); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: account deleted", ErrConflict)
		}
		return err
	}
	merged, err := merge(rec.SharedBaseline, raw, current)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `update wb_accounts set data = $2::jsonb, updated_at = now() where uid = $1`, rec.UID, string(merged)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	// Published account records are immutable. The caller reloads the committed
	// row into its pool; mutating this shared baseline would race other readers.
	return nil
}

// merge applies only fields changed relative to the observed snapshot. Compare
// decoded values so PostgreSQL jsonb whitespace/key ordering is immaterial.
func merge(baseline, proposed, current []byte) ([]byte, error) {
	decodeMap := func(raw []byte) (map[string]any, error) {
		var m map[string]any
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		err := d.Decode(&m)
		return m, err
	}
	before, err := decodeMap(baseline)
	if err != nil {
		return nil, err
	}
	next, err := decodeMap(proposed)
	if err != nil {
		return nil, err
	}
	latest, err := decodeMap(current)
	if err != nil {
		return nil, err
	}
	keys := make(map[string]bool)
	for k := range before {
		keys[k] = true
	}
	for k := range next {
		keys[k] = true
	}
	for k := range keys {
		old, had := before[k]
		value, has := next[k]
		if had == has && reflect.DeepEqual(old, value) {
			continue
		}
		live, exists := latest[k]
		if !(had == exists && reflect.DeepEqual(old, live)) && !(has == exists && reflect.DeepEqual(value, live)) {
			return nil, fmt.Errorf("%w: field %s", ErrConflict, k)
		}
		if has {
			latest[k] = value
		} else {
			delete(latest, k)
		}
	}
	return json.Marshal(latest)
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
