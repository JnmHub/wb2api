// Package pgsessions stores dashboard login sessions in PostgreSQL so every
// gateway replica sees the same device list and revocations.
package pgsessions

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"wb2api/internal/panelauth"
	"wb2api/internal/platform/pg"
)

// Store is a panelauth.Backend backed by the wb_panel_sessions table.
type Store struct {
	pool *pg.Pool
}

// New wraps the shared PostgreSQL pool. It returns nil for a nil pool.
func New(pool *pg.Pool) *Store {
	if pool == nil {
		return nil
	}
	return &Store{pool: pool}
}

const putSQL = `
	insert into wb_panel_sessions (id, csrf, created, last_seen, expires, remember, user_agent, ip)
	values ($1,$2,$3,$4,$5,$6,$7,$8)
	on conflict (id) do update set
		csrf = excluded.csrf,
		created = excluded.created,
		last_seen = excluded.last_seen,
		expires = excluded.expires,
		remember = excluded.remember,
		user_agent = excluded.user_agent,
		ip = excluded.ip`

// Separate from schema, settings and account-seed locks: only panel-session
// importers must serialize the marker read with the import and marker commit.
const fileImportLock int64 = 864235710257

// MigrateFile imports panel_sessions.json once. A marker in wb_panel_meta makes
// the import idempotent even after every session has been revoked, so a restart
// cannot resurrect a logged-out browser.
func (s *Store) MigrateFile(dir string) (int, error) {
	if s == nil || s.pool == nil {
		return 0, errors.New("postgres session store not configured")
	}
	ctx, cancel := s.queryContext()
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, fileImportLock); err != nil {
		return 0, err
	}
	var done bool
	if err := tx.QueryRow(ctx, `select exists(select 1 from wb_panel_meta where key = $1)`,
		"panel_file_sessions_imported").Scan(&done); err != nil {
		return 0, err
	}
	if done {
		return 0, tx.Commit(ctx)
	}
	list, err := panelauth.ReadFileSessions(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, ss := range list {
		if len(ss.ID) < 12 {
			continue
		}
		tag, err := tx.Exec(ctx, `
			insert into wb_panel_sessions (id, csrf, created, last_seen, expires, remember, user_agent, ip)
			values ($1,$2,$3,$4,$5,$6,$7,$8)
			on conflict (id) do nothing`,
			ss.ID, ss.CSRF, ss.Created, ss.LastSeen, ss.Expires, ss.Remember, ss.UserAgent, ss.IP)
		if err != nil {
			return 0, err
		}
		n += int(tag.RowsAffected())
	}
	if _, err := tx.Exec(ctx, `
		insert into wb_panel_meta (key, value) values ($1, $2)
		on conflict (key) do update set value = excluded.value`,
		"panel_file_sessions_imported", "1"); err != nil {
		return 0, err
	}
	return n, tx.Commit(ctx)
}

func (s *Store) queryContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// Get returns one session by its token hash.
func (s *Store) Get(id string) (panelauth.Session, bool, error) {
	if s == nil || s.pool == nil {
		return panelauth.Session{}, false, errors.New("postgres session store not configured")
	}
	ctx, cancel := s.queryContext()
	defer cancel()
	var ss panelauth.Session
	err := s.pool.QueryRow(ctx, `
		select id, csrf, created, last_seen, expires, remember, user_agent, ip
		from wb_panel_sessions where id = $1`, id).
		Scan(&ss.ID, &ss.CSRF, &ss.Created, &ss.LastSeen, &ss.Expires, &ss.Remember, &ss.UserAgent, &ss.IP)
	if errors.Is(err, pgx.ErrNoRows) {
		return panelauth.Session{}, false, nil
	}
	if err != nil {
		return panelauth.Session{}, false, err
	}
	return ss, true, nil
}

// List removes expired rows and returns the live sessions.
func (s *Store) List() ([]panelauth.Session, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("postgres session store not configured")
	}
	ctx, cancel := s.queryContext()
	defer cancel()
	if _, err := s.pool.Exec(ctx, `delete from wb_panel_sessions where expires <= now()`); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		select id, csrf, created, last_seen, expires, remember, user_agent, ip
		from wb_panel_sessions order by last_seen desc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []panelauth.Session
	for rows.Next() {
		var ss panelauth.Session
		if err := rows.Scan(&ss.ID, &ss.CSRF, &ss.Created, &ss.LastSeen, &ss.Expires, &ss.Remember, &ss.UserAgent, &ss.IP); err != nil {
			return nil, err
		}
		out = append(out, ss)
	}
	return out, rows.Err()
}

// Touch extends an existing session without recreating a revoked row.
func (s *Store) Touch(id string, lastSeen, expires time.Time) (panelauth.Session, bool, error) {
	if s == nil || s.pool == nil {
		return panelauth.Session{}, false, errors.New("postgres session store not configured")
	}
	ctx, cancel := s.queryContext()
	defer cancel()
	var ss panelauth.Session
	err := s.pool.QueryRow(ctx, `
		update wb_panel_sessions set last_seen = $2, expires = $3
		where id = $1 and expires > now()
		returning id, csrf, created, last_seen, expires, remember, user_agent, ip`, id, lastSeen, expires).
		Scan(&ss.ID, &ss.CSRF, &ss.Created, &ss.LastSeen, &ss.Expires, &ss.Remember, &ss.UserAgent, &ss.IP)
	if errors.Is(err, pgx.ErrNoRows) {
		return panelauth.Session{}, false, nil
	}
	if err != nil {
		return panelauth.Session{}, false, err
	}
	return ss, true, nil
}

// Put creates or updates one session.
func (s *Store) Put(ss panelauth.Session) error {
	if s == nil || s.pool == nil {
		return errors.New("postgres session store not configured")
	}
	ctx, cancel := s.queryContext()
	defer cancel()
	_, err := s.pool.Exec(ctx, putSQL,
		ss.ID, ss.CSRF, ss.Created, ss.LastSeen, ss.Expires, ss.Remember, ss.UserAgent, ss.IP)
	return err
}

// Delete ends one session.
func (s *Store) Delete(id string) error {
	if s == nil || s.pool == nil {
		return errors.New("postgres session store not configured")
	}
	ctx, cancel := s.queryContext()
	defer cancel()
	_, err := s.pool.Exec(ctx, `delete from wb_panel_sessions where id = $1`, id)
	return err
}

// DeleteAllExcept ends every session except keep ("" = all).
func (s *Store) DeleteAllExcept(keep string) (int, error) {
	if s == nil || s.pool == nil {
		return 0, errors.New("postgres session store not configured")
	}
	ctx, cancel := s.queryContext()
	defer cancel()
	var (
		tag pgconn.CommandTag
		err error
	)
	if keep == "" {
		tag, err = s.pool.Exec(ctx, `delete from wb_panel_sessions`)
	} else {
		tag, err = s.pool.Exec(ctx, `delete from wb_panel_sessions where id <> $1`, keep)
	}
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
