// Package leader elects one wb2api replica for background work. The lock is a
// PostgreSQL session advisory lock: it disappears with the connection, so a
// crashed leader never blocks the next replica.
package leader

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// KeyBackground is the advisory-lock key used by the scheduler, quota probes
// and other unattended jobs. The value is ASCII "wb2api-b".
const KeyBackground int64 = 0x7762326170692d62

// KeyImport serializes the one-time legacy history import across replicas.
// The value is ASCII "wb2api-i". Without it two replicas can read the same
// resume offset and insert the same JSONL/SQLite rows twice.
const KeyImport int64 = 0x7762326170692d69

// WithLock runs fn while holding a PostgreSQL session advisory lock. A nil pool
// (standalone/file mode) runs fn directly. Callers block until the lock is
// available, so a replica that starts while another is importing waits instead
// of skipping the import forever.
func WithLock(ctx context.Context, pool *pgxpool.Pool, key int64, fn func() error) error {
	if pool == nil {
		return fn()
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `select pg_advisory_lock($1)`, key); err != nil {
		return err
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `select pg_advisory_unlock($1)`, key)
	}()
	return fn()
}

// Elector keeps a session advisory lock while it is the leader.
type Elector struct {
	pool *pgxpool.Pool
	key  int64
	log  *slog.Logger
	live atomic.Bool
}

// New returns an elector. A nil pool makes every replica a leader, preserving
// the standalone/file mode.
func New(pool *pgxpool.Pool, key int64, log *slog.Logger) *Elector {
	return &Elector{pool: pool, key: key, log: log}
}

// IsLeader reports whether this process currently owns the lock.
func (e *Elector) IsLeader() bool {
	return e == nil || e.pool == nil || e.live.Load()
}

// Run acquires and holds the lock until ctx ends. When the connection dies the
// lock is lost and the loop tries to acquire it again.
func (e *Elector) Run(ctx context.Context) {
	if e == nil || e.pool == nil {
		return
	}
	for {
		if ctx.Err() != nil {
			return
		}
		conn, err := e.pool.Acquire(ctx)
		if err != nil {
			if !sleep(ctx, 2*time.Second) {
				return
			}
			continue
		}
		var locked bool
		err = conn.QueryRow(ctx, `select pg_try_advisory_lock($1)`, e.key).Scan(&locked)
		if err != nil || !locked {
			conn.Release()
			if !sleep(ctx, 2*time.Second) {
				return
			}
			continue
		}
		e.live.Store(true)
		if e.log != nil {
			e.log.Info("background leader acquired", "key", e.key)
		}
		e.hold(ctx, conn)
		e.live.Store(false)
		conn.Release()
	}
}

func (e *Elector) hold(ctx context.Context, conn *pgxpool.Conn) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			e.unlock(conn)
			return
		case <-t.C:
			pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				if e.log != nil {
					e.log.Warn("background leader lost its database connection", "err", err)
				}
				e.unlock(conn)
				return
			}
		}
	}
}

func (e *Elector) unlock(conn *pgxpool.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = conn.Exec(ctx, `select pg_advisory_unlock($1)`, e.key)
	if e.log != nil {
		e.log.Info("background leader released", "key", e.key)
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}
