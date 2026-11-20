package pgaccounts

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LockRaccoonAccount coordinates rotating refreshes and task side effects
// between replicas. The dedicated connection leaves the app pool available
// for account reloads and persistence while the upstream request is running.
func LockRaccoonAccount(ctx context.Context, db *pgxpool.Pool, scope, uid string) (func(), error) {
	conn, err := pgx.ConnectConfig(ctx, db.Config().ConnConfig.Copy())
	if err != nil {
		return nil, err
	}
	release := func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = conn.Close(cleanup)
	}
	tx, err := conn.Begin(ctx)
	if err == nil {
		_, err = tx.Exec(ctx, "set local idle_in_transaction_session_timeout = 0")
	}
	if err == nil {
		_, err = tx.Exec(ctx, "select pg_advisory_xact_lock(hashtextextended($1, 0))", "wb2api:raccoon:"+scope+":"+uid)
	}
	if err != nil {
		release()
		return nil, fmt.Errorf("raccoon account lock: %w", err)
	}
	return release, nil
}
