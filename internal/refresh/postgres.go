package refresh

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"wb2api/internal/store/accounts"
	"wb2api/internal/store/pgaccounts"
)

// SetCodeArtsPostgres coordinates exchanges and STS pacing across hub
// replicas that share the account store.
func (r *Refresher) SetCodeArtsPostgres(db *pgxpool.Pool) {
	// Bound the number of pinned sessions, leaving capacity for account
	// reconciliation, persistence and the request path.
	capacity := min(4, db.Config().MaxConns/2)
	slots := make(chan struct{}, max(1, capacity))
	r.LockCodeArts = func(ctx context.Context, uid string) (func(), error) {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			<-slots
			return nil, err
		}
		release := func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(cleanup)
			<-slots
		}
		if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock(hashtextextended($1,0))", "wb2api-codearts-refresh:"+uid); err != nil {
			release()
			return nil, err
		}
		return release, nil
	}
	r.LoadLatest = func(ctx context.Context, uid string) (*accounts.Record, error) {
		return pgaccounts.LoadOne(ctx, db, uid)
	}
	r.WaitCodeArtsSTS = func(ctx context.Context, egress string) error {
		for {
			var micros int64
			err := db.QueryRow(ctx, `
				with permit as (
					insert into wb_codearts_sts_egress as e (egress, next_at)
						values ($1, clock_timestamp() + $2::bigint * interval '1 microsecond')
					on conflict (egress) do update set
						next_at = clock_timestamp() + $2::bigint * interval '1 microsecond'
					where e.next_at <= clock_timestamp()
					returning 0::bigint as wait_us
				)
				select coalesce((select wait_us from permit), (
					select greatest(1000::bigint, cast(extract(epoch from next_at - clock_timestamp()) * 1000000 as bigint))
					from wb_codearts_sts_egress where egress = $1
				), $2::bigint)
			`, egress, codeArtsSpacing.Microseconds()).Scan(&micros)
			if err != nil {
				return err
			}
			if micros == 0 {
				return nil
			}
			if err := waitForSTS(ctx, time.Duration(micros)*time.Microsecond); err != nil {
				return err
			}
		}
	}
}
