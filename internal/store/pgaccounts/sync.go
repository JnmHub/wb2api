package pgaccounts

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"wb2api/internal/store/accounts"
)

// LoadSnapshot returns an authoritative account set and its transactionally
// coherent watermark. Seed import, if requested, precedes the snapshot. Use the
// returned cursor with Changes; never derive a cursor from timestamps or a
// separate Load call. The migration's transactional clock makes late commits
// impossible to skip, and repeatable read keeps the clock and rows coherent.
func LoadSnapshot(ctx context.Context, pool *pgxpool.Pool, seedDir string) ([]*accounts.Record, []accounts.LoadError, int64, error) {
	if pool == nil {
		return nil, nil, 0, errors.New("postgres not configured")
	}
	if seedDir != "" {
		if err := seedOnce(ctx, pool, seedDir); err != nil {
			return nil, nil, 0, err
		}
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, nil, 0, err
	}
	defer tx.Rollback(ctx)
	// TRUNCATE is not MVCC-safe. Take the ordinary table read lock before
	// establishing the snapshot so it cannot empty rows behind an older clock.
	if _, err := tx.Exec(ctx, `lock table wb_accounts in access share mode`); err != nil {
		return nil, nil, 0, err
	}
	var cursor int64
	if err := tx.QueryRow(ctx, `select version from wb_account_sync_clock where id`).Scan(&cursor); err != nil {
		return nil, nil, 0, err
	}
	rows, err := tx.Query(ctx, `select uid, data from wb_accounts order by uid`)
	if err != nil {
		return nil, nil, 0, err
	}
	recs, bad, err := readRecords(rows)
	if err != nil {
		return nil, nil, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, 0, err
	}
	return recs, bad, cursor, nil
}

// ErrCursorAhead means this cursor belongs to a newer database state (for
// example after a restore). The caller must replace it with LoadSnapshot.
var ErrCursorAhead = errors.New("account sync cursor ahead of database")

// Changes returns final per-UID states changed since cursor, tombstones, decode
// errors, and the new coherent cursor. Entries are compacted by UID, not an
// ever-growing event log; tombstones must not be pruned while cursors may exist.
// A malformed live row is also returned as a deletion (fail closed), so callers
// may log bad and advance the cursor without retaining a revoked stale account.
// A cursor ahead of this database (restore/reset) is rejected: take a new full
// snapshot instead. Only advance a cursor after applying the complete result.
func Changes(ctx context.Context, pool *pgxpool.Pool, cursor int64) ([]*accounts.Record, []string, []accounts.LoadError, int64, error) {
	if pool == nil {
		return nil, nil, nil, cursor, errors.New("postgres not configured")
	}
	if cursor < 0 {
		return nil, nil, nil, cursor, errors.New("invalid account sync cursor")
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, nil, nil, cursor, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `lock table wb_accounts in access share mode`); err != nil {
		return nil, nil, nil, cursor, err
	}
	var next int64
	if err := tx.QueryRow(ctx, `select version from wb_account_sync_clock where id`).Scan(&next); err != nil {
		return nil, nil, nil, cursor, err
	}
	if cursor > next {
		return nil, nil, nil, cursor, fmt.Errorf("%w: cursor %d, database %d; reload snapshot", ErrCursorAhead, cursor, next)
	}
	if cursor == next {
		return nil, nil, nil, next, tx.Commit(ctx)
	}
	rows, err := tx.Query(ctx, `
		select c.uid, c.deleted or a.uid is null, a.data
		from wb_account_changes c left join wb_accounts a on a.uid=c.uid
		where c.version > $1 and c.version <= $2 order by c.version,c.uid`, cursor, next)
	if err != nil {
		return nil, nil, nil, cursor, err
	}
	defer rows.Close()
	var recs []*accounts.Record
	var deleted []string
	var bad []accounts.LoadError
	now := time.Now()
	for rows.Next() {
		var uid string
		var removed bool
		var raw []byte
		if err := rows.Scan(&uid, &removed, &raw); err != nil {
			return nil, nil, nil, cursor, err
		}
		if removed {
			deleted = append(deleted, uid)
			continue
		}
		rec, err := decode(uid, raw, now)
		if err != nil {
			bad = append(bad, accounts.LoadError{File: "postgres:" + uid, Err: err})
			deleted = append(deleted, uid)
			continue
		}
		recs = append(recs, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, cursor, err
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, nil, cursor, err
	}
	return recs, deleted, bad, next, nil
}

// LoadOne reloads the latest merged row after Save without a full-table scan.
// It returns pgx.ErrNoRows when a concurrent delete won. It never advances a
// polling cursor; the next Changes must still observe unrelated remote writes.
func LoadOne(ctx context.Context, pool *pgxpool.Pool, uid string) (*accounts.Record, error) {
	if pool == nil {
		return nil, errors.New("postgres not configured")
	}
	var raw []byte
	if err := pool.QueryRow(ctx, `select data from wb_accounts where uid=$1`, uid).Scan(&raw); err != nil {
		return nil, err
	}
	return decode(uid, raw, time.Now())
}

func readRecords(rows pgx.Rows) ([]*accounts.Record, []accounts.LoadError, error) {
	defer rows.Close()
	out := make([]*accounts.Record, 0)
	var bad []accounts.LoadError
	now := time.Now()
	for rows.Next() {
		var uid string
		var raw []byte
		if err := rows.Scan(&uid, &raw); err != nil {
			return nil, nil, err
		}
		rec, err := decode(uid, raw, now)
		if err != nil {
			bad = append(bad, accounts.LoadError{File: "postgres:" + uid, Err: err})
			continue
		}
		out = append(out, rec)
	}
	return out, bad, rows.Err()
}
