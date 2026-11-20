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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"wb2api/internal/realm"
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
	}
	if err := protectRaccoonRows(ctx, pool); err != nil {
		return nil, nil, err
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
				raw, e := rec.MarshalPostgres()
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
	m, err := accounts.UnwrapStoredRecord(m)
	if err != nil {
		return nil, err
	}
	if v, _ := m["uid"].(string); v != "" && v != uid {
		// Tombstones are keyed by the SQL primary key. Publishing a different
		// embedded UID would leave an undeletable stale credential in the pool.
		return nil, fmt.Errorf("account uid %q does not match database key %q", v, uid)
	}
	m["uid"] = uid
	rec := accounts.FromMap(m, "", now)
	baseline, err := rec.Marshal()
	if err != nil {
		return nil, err
	}
	rec.SharedBaseline = baseline
	return rec, nil
}

// ErrConflict means another replica changed a field this save also changed.
var ErrConflict = accounts.ErrConflict

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
		if rec.ObservedRaccoonCredential != nil {
			return fmt.Errorf("%w: Raccoon write has no persisted baseline", ErrConflict)
		}
		stored, err := rec.MarshalPostgres()
		if err != nil {
			return err
		}
		result, err := pool.Exec(ctx, `
			insert into wb_accounts(uid, data, updated_at) values($1, $2::jsonb, now())
			on conflict (uid) do update set data = excluded.data, updated_at = now()
			where not $3 and not (wb_accounts.data ? 'raccoon_record_v1')
			  and lower(btrim(coalesce(wb_accounts.data->>'realm',''))) <> 'raccoon'`, rec.UID, string(stored), realm.IsRaccoon(rec.Realm))
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return fmt.Errorf("%w: protected account UID already exists", ErrConflict)
		}
		rec.SharedBaseline = raw
		return nil
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Match the trigger's lock order: clock before account row. Otherwise two
	// cooperating Save transactions can deadlock on different account rows when
	// a transaction writes more than one account. The lock is held until commit.
	if _, err := tx.Exec(ctx, `select version from wb_account_sync_clock where id for update`); err != nil {
		return err
	}
	var current []byte
	if err := tx.QueryRow(ctx, `select data from wb_accounts where uid = $1 for update`, rec.UID).Scan(&current); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: account deleted", ErrConflict)
		}
		return err
	}
	if realm.IsRaccoon(rec.Realm) {
		live, err := decode(rec.UID, current, time.Now())
		if err != nil {
			return err
		}
		if live.Realm != rec.Realm {
			return fmt.Errorf("%w: account channel changed", ErrConflict)
		}
		if rec.ObservedRaccoonCredential != nil && !live.MatchesRaccoonCredential(rec.ObservedRaccoonCredential) {
			return fmt.Errorf("%w: Raccoon credential changed before commit", ErrConflict)
		}
		current = live.SharedBaseline
	}
	merged, err := merge(rec.SharedBaseline, raw, current)
	if err != nil {
		return err
	}
	if realm.IsRaccoon(rec.Realm) {
		next, err := decode(rec.UID, merged, time.Now())
		if err != nil {
			return err
		}
		merged, err = next.MarshalPostgres()
		if err != nil {
			return err
		}
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

func merge(baseline, proposed, current []byte) ([]byte, error) {
	return accounts.MergeJSON(baseline, proposed, current)
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
