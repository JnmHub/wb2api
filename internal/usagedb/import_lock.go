package usagedb

import (
	"context"
	"database/sql"
)

// Serialize imports with the replay/rebuild writer on PG. SQLite's writer
// transaction already provides this exclusion. Existing import offset and row
// changes remain in the same transaction; do not rebuild from sampled details.
func (d *DB) beginImport() (*sql.Tx, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, err
	}
	if d.driver == "pg" {
		ctx, cancel := context.WithTimeout(context.Background(), d.writeTimeout)
		defer cancel()
		if _, err = tx.ExecContext(ctx, "select pg_advisory_xact_lock($1)", pgUsageWriterLock); err != nil {
			tx.Rollback()
			return nil, err
		}
	}
	return tx, nil
}
