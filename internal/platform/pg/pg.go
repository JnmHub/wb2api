// Package pg owns the PostgreSQL connection pool and the small migration set
// used by the account/settings stores. It intentionally stays free of any
// gateway logic so both the file-backed and database-backed modes can share it.
package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool is the shared PostgreSQL handle.
type Pool struct {
	*pgxpool.Pool
}

// SchemaLockKey serializes schema creation across replicas. PostgreSQL's
// CREATE TABLE IF NOT EXISTS is not safe against two sessions creating the same
// table at once: both can try to create the composite row type and one fails
// with a duplicate pg_type error. The usage store uses the same key.
const SchemaLockKey int64 = 0x7762326170692d73 // "wb2api-s"

// Open connects, verifies the database and applies the schema.
func Open(ctx context.Context, dsn string) (*Pool, error) {
	if dsn == "" {
		return nil, errors.New("empty postgres dsn")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres dsn: %w", err)
	}
	cfg.MaxConns = 20
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres connect: %w", err)
	}
	if err := p.Ping(ctx); err != nil {
		p.Close()
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	if err := withSchemaLock(ctx, p, func(conn *pgxpool.Conn) error { return migrate(ctx, conn) }); err != nil {
		p.Close()
		return nil, fmt.Errorf("postgres migrate: %w", err)
	}
	return &Pool{Pool: p}, nil
}

// withSchemaLock holds a session advisory lock while fn creates or upgrades
// tables. Each replica must use the same key, otherwise simultaneous startup
// can race on PostgreSQL's internal pg_type rows.
func withSchemaLock(ctx context.Context, p *pgxpool.Pool, fn func(*pgxpool.Conn) error) error {
	conn, err := p.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "select pg_advisory_lock($1)", SchemaLockKey); err != nil {
		return err
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, "select pg_advisory_unlock($1)", SchemaLockKey)
	}()
	return fn(conn)
}

const schema = `
create table if not exists wb_accounts (
	uid text primary key,
	data jsonb not null,
	updated_at timestamptz not null default now()
);
create table if not exists wb_settings (
	id boolean primary key default true check (id),
	data jsonb not null,
	updated_at timestamptz not null default now()
);
create table if not exists wb_panel_sessions (
	id text primary key,
	csrf text not null,
	created timestamptz not null,
	last_seen timestamptz not null,
	expires timestamptz not null,
	remember boolean not null default false,
	user_agent text not null default '',
	ip text not null default ''
);
create index if not exists wb_panel_sessions_expires_idx on wb_panel_sessions (expires);
create table if not exists wb_panel_meta (
	key text primary key,
	value text not null
);
create table if not exists wb_state (
	key text primary key,
	data jsonb not null,
	updated_at timestamptz not null default now()
);
create table if not exists wb_jobs (
	id text primary key,
	data jsonb not null,
	updated_at timestamptz not null default now()
);
create index if not exists wb_jobs_updated_idx on wb_jobs (updated_at desc);
`

func migrate(ctx context.Context, conn *pgxpool.Conn) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, schema); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, accountSyncSchema); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Ping is used by /health without blocking the request path for long.
func (p *Pool) Ping(ctx context.Context) error {
	if p == nil || p.Pool == nil {
		return errors.New("postgres not configured")
	}
	return p.Pool.Ping(ctx)
}
