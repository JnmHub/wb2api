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
	if err := migrate(ctx, p); err != nil {
		p.Close()
		return nil, fmt.Errorf("postgres migrate: %w", err)
	}
	return &Pool{Pool: p}, nil
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
`

func migrate(ctx context.Context, p *pgxpool.Pool) error {
	_, err := p.Exec(ctx, schema)
	return err
}

// Ping is used by /health without blocking the request path for long.
func (p *Pool) Ping(ctx context.Context) error {
	if p == nil || p.Pool == nil {
		return errors.New("postgres not configured")
	}
	return p.Pool.Ping(ctx)
}
