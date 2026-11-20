package pgaccounts

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// protectRaccoonRows moves records written by pre-release builds out of the
// format understood by an older gateway. Apply the same transactional clock
// order as Save so peers observe the new encoding through regular sync.
func protectRaccoonRows(ctx context.Context, db *pgxpool.Pool) error {
	const predicate = `lower(btrim(data->>'realm')) = 'raccoon'`
	var found bool
	if err := db.QueryRow(ctx, `select exists(select 1 from wb_accounts where `+predicate+`)`).Scan(&found); err != nil || !found {
		return err
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select version from wb_account_sync_clock where id for update`); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `select uid,data from wb_accounts where `+predicate+` order by uid for update`)
	if err != nil {
		return err
	}
	type convertedRow struct {
		uid string
		raw []byte
	}
	var converted []convertedRow
	for rows.Next() {
		var row convertedRow
		if err := rows.Scan(&row.uid, &row.raw); err != nil {
			rows.Close()
			return err
		}
		record, err := decode(row.uid, row.raw, time.Now())
		if err != nil {
			rows.Close()
			return fmt.Errorf("decode Raccoon account for migration: %w", err)
		}
		row.raw, err = record.MarshalPostgres()
		if err != nil {
			rows.Close()
			return err
		}
		converted = append(converted, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, row := range converted {
		if _, err := tx.Exec(ctx, `update wb_accounts set data=$2::jsonb, updated_at=now() where uid=$1`, row.uid, string(row.raw)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
