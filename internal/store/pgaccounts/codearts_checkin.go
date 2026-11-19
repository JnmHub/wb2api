package pgaccounts

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WithCodeArtsCheckinLock fences claim and confirmation writes across replicas.
// Use a dedicated connection so the locked job can still load, refresh and
// commit its account through the ordinary pool, even with pool_max_conns=1.
func WithCodeArtsCheckinLock(ctx context.Context, pool *pgxpool.Pool, uid string, run func(context.Context) error) error {
	if pool == nil {
		return errors.New("postgres not configured")
	}
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	// The bounded caller spends most of the transaction in upstream HTTP I/O.
	// Keep the fence for the whole operation even if the server uses a short
	// idle transaction timeout for other connections.
	if _, err := tx.Exec(ctx, "set local idle_in_transaction_session_timeout = 0"); err != nil {
		return err
	}
	var acquired bool
	if err := tx.QueryRow(ctx, "select pg_try_advisory_xact_lock(hashtextextended($1, 0))", "wb2api:codearts-checkin:"+uid).Scan(&acquired); err != nil {
		return err
	}
	if !acquired {
		return errors.New("另一副本正在执行签到")
	}
	// This transaction only owns the fence. Ledger writes use the ordinary
	// pool and remain committed even when closing an expired request context.
	return run(ctx)
}
