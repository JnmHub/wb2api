// Package usagedb stores request usage in SQLite and keeps hourly rollups up to
// date as rows are written, so dashboards read small pre-aggregated tables
// instead of re-scanning a growing log (the legacy design: seconds to tens of
// seconds of CPU on every cold start).
//
// Writes are asynchronous: the request path only enqueues an Event; a single
// goroutine inserts batches inside one transaction. When the queue is full the
// event is dropped and counted rather than blocking a response.
package usagedb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL driver for the shared usage store
	_ "modernc.org/sqlite"             // pure-Go SQLite driver (no cgo)
)

// Event is one finished request.
type Event struct {
	At         time.Time
	Model      string
	Account    string
	Realm      string
	KeyID      string
	Stream     bool
	Outcome    string // completed | client_aborted | upstream_aborted | failed
	Status     int
	ElapsedMs  int64
	TTFTMs     *int64
	GenMs      *int64
	HeldMs     int64
	Prompt     int64
	Completion int64
	Reasoning  int64
	Cached     int64
	Total      int64
	Credit     float64
	Effort     string
	Estimated  bool // token figures were estimated locally
	Error      string
	Extra      json.RawMessage // legacy fields without a column (imported rows)
}

const schema = `
CREATE TABLE IF NOT EXISTS requests (
  id          INTEGER PRIMARY KEY,
  at          REAL    NOT NULL,
  model       TEXT    NOT NULL DEFAULT '',
  account     TEXT    NOT NULL DEFAULT '',
  realm       TEXT    NOT NULL DEFAULT '',
  key_id      TEXT    NOT NULL DEFAULT '',
  stream      INTEGER NOT NULL DEFAULT 0,
  outcome     TEXT    NOT NULL DEFAULT '',
  status      INTEGER NOT NULL DEFAULT 0,
  elapsed_ms  INTEGER NOT NULL DEFAULT 0,
  ttft_ms     INTEGER,
  gen_ms      INTEGER,
  held_ms     INTEGER NOT NULL DEFAULT 0,
  prompt      INTEGER NOT NULL DEFAULT 0,
  completion  INTEGER NOT NULL DEFAULT 0,
  reasoning   INTEGER NOT NULL DEFAULT 0,
  cached      INTEGER NOT NULL DEFAULT 0,
  total       INTEGER NOT NULL DEFAULT 0,
  credit      REAL    NOT NULL DEFAULT 0,
  effort      TEXT    NOT NULL DEFAULT '',
  estimated   INTEGER NOT NULL DEFAULT 0,
  error       TEXT    NOT NULL DEFAULT '',
  extra       TEXT
);
CREATE INDEX IF NOT EXISTS requests_at          ON requests(at);
CREATE INDEX IF NOT EXISTS requests_account_at  ON requests(account, at);
CREATE INDEX IF NOT EXISTS requests_model_at    ON requests(model, at);
CREATE INDEX IF NOT EXISTS requests_key_at      ON requests(key_id, at);

CREATE TABLE IF NOT EXISTS daily (
  day         TEXT    NOT NULL,           -- local date, YYYY-MM-DD
  realm       TEXT    NOT NULL,
  model       TEXT    NOT NULL,
  account     TEXT    NOT NULL,
  key_id      TEXT    NOT NULL,
  requests    INTEGER NOT NULL DEFAULT 0,
  ok          INTEGER NOT NULL DEFAULT 0,
  aborted     INTEGER NOT NULL DEFAULT 0,
  failed      INTEGER NOT NULL DEFAULT 0,
  prompt      INTEGER NOT NULL DEFAULT 0,
  completion  INTEGER NOT NULL DEFAULT 0,
  reasoning   INTEGER NOT NULL DEFAULT 0,
  cached      INTEGER NOT NULL DEFAULT 0,
  total       INTEGER NOT NULL DEFAULT 0,
  credit      REAL    NOT NULL DEFAULT 0,
  elapsed_sum INTEGER NOT NULL DEFAULT 0,
  ttft_sum    INTEGER NOT NULL DEFAULT 0,
  ttft_n      INTEGER NOT NULL DEFAULT 0,
  gen_ms_sum  INTEGER NOT NULL DEFAULT 0,
  gen_tokens  INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (day, realm, model, account, key_id)
) WITHOUT ROWID;

-- Hourly rollup for charts: only realm and model are kept per hour, so it stays
-- small (hours x models) however many accounts there are.
CREATE TABLE IF NOT EXISTS hourly (
  hour        INTEGER NOT NULL,
  realm       TEXT    NOT NULL,
  model       TEXT    NOT NULL,
  requests    INTEGER NOT NULL DEFAULT 0,
  ok          INTEGER NOT NULL DEFAULT 0,
  aborted     INTEGER NOT NULL DEFAULT 0,
  failed      INTEGER NOT NULL DEFAULT 0,
  prompt      INTEGER NOT NULL DEFAULT 0,
  completion  INTEGER NOT NULL DEFAULT 0,
  reasoning   INTEGER NOT NULL DEFAULT 0,
  cached      INTEGER NOT NULL DEFAULT 0,
  total       INTEGER NOT NULL DEFAULT 0,
  credit      REAL    NOT NULL DEFAULT 0,
  elapsed_sum INTEGER NOT NULL DEFAULT 0,
  ttft_sum    INTEGER NOT NULL DEFAULT 0,
  ttft_n      INTEGER NOT NULL DEFAULT 0,
  gen_ms_sum  INTEGER NOT NULL DEFAULT 0,
  gen_tokens  INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (hour, realm, model)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);
`

// DB is an open usage database. It speaks SQLite by default and PostgreSQL
// when opened with OpenPostgres; the schema and queries are shared.
type DB struct {
	sql    *sql.DB
	driver string // "sqlite" or "pg"

	queue   chan Event
	flushCh chan chan struct{}
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once

	dropped atomic.Int64
	written atomic.Int64
	lastErr atomic.Value // string
}

// Open opens (creating if needed) the database at path and starts the writer.
func Open(path string, queueSize int) (*DB, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(0)"
	h, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	h.SetMaxOpenConns(4)
	if _, err := h.Exec(schema); err != nil {
		h.Close()
		return nil, fmt.Errorf("usagedb schema: %w", err)
	}
	if queueSize <= 0 {
		queueSize = 8192
	}
	d := &DB{
		sql: h, driver: "sqlite", queue: make(chan Event, queueSize), flushCh: make(chan chan struct{}),
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	go d.writer()
	return d, nil
}

// OpenPostgres opens the usage store in PostgreSQL. The tables are created on
// first use in the given database; callers normally pass the same DSN as the
// account/settings store.
func OpenPostgres(dsn string, queueSize int) (*DB, error) {
	h, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	h.SetMaxOpenConns(8)
	h.SetMaxIdleConns(4)
	h.SetConnMaxLifetime(30 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.PingContext(ctx); err != nil {
		h.Close()
		return nil, fmt.Errorf("usagedb postgres ping: %w", err)
	}
	// pgx's extended protocol rejects multiple commands in one Exec, so run
	// each schema statement separately.
	for _, stmt := range strings.Split(postgresSchema(), ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := h.ExecContext(ctx, stmt); err != nil {
			h.Close()
			return nil, fmt.Errorf("usagedb postgres schema: %w", err)
		}
	}
	if queueSize <= 0 {
		queueSize = 8192
	}
	d := &DB{
		sql: h, driver: "pg", queue: make(chan Event, queueSize), flushCh: make(chan chan struct{}),
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	go d.writer()
	return d, nil
}

// postgresSchema reuses the SQLite schema with the minimal syntax changes:
// BIGSERIAL for the request id and no SQLite-only WITHOUT ROWID clause.
func postgresSchema() string {
	s := strings.ReplaceAll(schema, "id          INTEGER PRIMARY KEY", "id          BIGSERIAL PRIMARY KEY")
	s = strings.ReplaceAll(s, "REAL", "DOUBLE PRECISION")
	s = strings.ReplaceAll(s, ") WITHOUT ROWID", ")")
	return s
}

// bind rewrites SQLite-style ? placeholders to PostgreSQL $1..$n.
func (d *DB) bind(q string) string {
	if d.driver != "pg" {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	for i := 0; i < len(q); i++ {
		if q[i] != '?' {
			b.WriteByte(q[i])
			continue
		}
		n++
		fmt.Fprintf(&b, "$%d", n)
	}
	return b.String()
}

func (d *DB) query(q string, args ...any) (*sql.Rows, error) {
	return d.sql.Query(d.bind(q), args...)
}

func (d *DB) queryRow(q string, args ...any) *sql.Row {
	return d.sql.QueryRow(d.bind(q), args...)
}

func (d *DB) prepare(tx *sql.Tx, q string) (*sql.Stmt, error) {
	return tx.Prepare(d.bind(q))
}

// SQL exposes the handle for read-only queries by other packages and tests.
func (d *DB) SQL() *sql.DB { return d.sql }

// Dropped is the number of events discarded because the queue was full.
func (d *DB) Dropped() int64 { return d.dropped.Load() }

// Written is the number of events stored so far.
func (d *DB) Written() int64 { return d.written.Load() }

// LastError returns the last write error ("" when none).
func (d *DB) LastError() string {
	s, _ := d.lastErr.Load().(string)
	return s
}

// Record enqueues an event without ever blocking the caller.
func (d *DB) Record(e Event) {
	select {
	case d.queue <- e:
	default:
		d.dropped.Add(1)
	}
}

// Flush waits until everything enqueued so far is on disk.
func (d *DB) Flush() {
	ack := make(chan struct{})
	select {
	case d.flushCh <- ack:
		<-ack
	case <-d.done:
	}
}

// Close flushes and stops the writer.
func (d *DB) Close() error {
	var err error
	d.once.Do(func() {
		close(d.stop)
		<-d.done
		err = d.sql.Close()
	})
	return err
}

const (
	batchMax   = 256
	batchDelay = 150 * time.Millisecond
)

func (d *DB) writer() {
	defer close(d.done)
	batch := make([]Event, 0, batchMax)
	timer := time.NewTimer(batchDelay)
	defer timer.Stop()
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := d.writeBatch(batch); err != nil {
			d.lastErr.Store(err.Error())
		} else {
			d.written.Add(int64(len(batch)))
		}
		batch = batch[:0]
	}
	drain := func() {
		for {
			select {
			case e := <-d.queue:
				batch = append(batch, e)
				if len(batch) >= batchMax {
					flush()
				}
			default:
				return
			}
		}
	}
	for {
		select {
		case e := <-d.queue:
			batch = append(batch, e)
			if len(batch) >= batchMax {
				flush()
			}
		case <-timer.C:
			flush()
			timer.Reset(batchDelay)
		case ack := <-d.flushCh:
			drain()
			flush()
			close(ack)
		case <-d.stop:
			drain()
			flush()
			return
		}
	}
}

func nullInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

const insertSQL = `INSERT INTO requests (at, model, account, realm, key_id, stream, outcome, status, elapsed_ms, ttft_ms, gen_ms, held_ms,
  prompt, completion, reasoning, cached, total, credit, effort, estimated, error, extra)
  VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

const upsertDailySQL = `INSERT INTO daily (day, realm, model, account, key_id, requests, ok, aborted, failed, prompt, completion, reasoning,
  cached, total, credit, elapsed_sum, ttft_sum, ttft_n, gen_ms_sum, gen_tokens)
  VALUES (?,?,?,?,?,1,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
  ON CONFLICT(day, realm, model, account, key_id) DO UPDATE SET
    requests=requests+1, ok=ok+excluded.ok, aborted=aborted+excluded.aborted, failed=failed+excluded.failed,
    prompt=prompt+excluded.prompt, completion=completion+excluded.completion, reasoning=reasoning+excluded.reasoning,
    cached=cached+excluded.cached, total=total+excluded.total, credit=credit+excluded.credit,
    elapsed_sum=elapsed_sum+excluded.elapsed_sum, ttft_sum=ttft_sum+excluded.ttft_sum, ttft_n=ttft_n+excluded.ttft_n,
    gen_ms_sum=gen_ms_sum+excluded.gen_ms_sum, gen_tokens=gen_tokens+excluded.gen_tokens`

const upsertHourlySQL = `INSERT INTO hourly (hour, realm, model, requests, ok, aborted, failed, prompt, completion, reasoning,
  cached, total, credit, elapsed_sum, ttft_sum, ttft_n, gen_ms_sum, gen_tokens)
  VALUES (?,?,?,1,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
  ON CONFLICT(hour, realm, model) DO UPDATE SET
    requests=requests+1, ok=ok+excluded.ok, aborted=aborted+excluded.aborted, failed=failed+excluded.failed,
    prompt=prompt+excluded.prompt, completion=completion+excluded.completion, reasoning=reasoning+excluded.reasoning,
    cached=cached+excluded.cached, total=total+excluded.total, credit=credit+excluded.credit,
    elapsed_sum=elapsed_sum+excluded.elapsed_sum, ttft_sum=ttft_sum+excluded.ttft_sum, ttft_n=ttft_n+excluded.ttft_n,
    gen_ms_sum=gen_ms_sum+excluded.gen_ms_sum, gen_tokens=gen_tokens+excluded.gen_tokens`

// PostgreSQL requires the target columns in ON CONFLICT DO UPDATE to be
// qualified when the same names also appear in excluded; SQLite rejects the
// qualified form, so keep separate statements for each dialect.
const pgUpsertDailySQL = `INSERT INTO daily (day, realm, model, account, key_id, requests, ok, aborted, failed, prompt, completion, reasoning,
  cached, total, credit, elapsed_sum, ttft_sum, ttft_n, gen_ms_sum, gen_tokens)
  VALUES ($1,$2,$3,$4,$5,1,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
  ON CONFLICT(day, realm, model, account, key_id) DO UPDATE SET
    requests=daily.requests+1, ok=daily.ok+excluded.ok, aborted=daily.aborted+excluded.aborted, failed=daily.failed+excluded.failed,
    prompt=daily.prompt+excluded.prompt, completion=daily.completion+excluded.completion, reasoning=daily.reasoning+excluded.reasoning,
    cached=daily.cached+excluded.cached, total=daily.total+excluded.total, credit=daily.credit+excluded.credit,
    elapsed_sum=daily.elapsed_sum+excluded.elapsed_sum, ttft_sum=daily.ttft_sum+excluded.ttft_sum, ttft_n=daily.ttft_n+excluded.ttft_n,
    gen_ms_sum=daily.gen_ms_sum+excluded.gen_ms_sum, gen_tokens=daily.gen_tokens+excluded.gen_tokens`

const pgUpsertHourlySQL = `INSERT INTO hourly (hour, realm, model, requests, ok, aborted, failed, prompt, completion, reasoning,
  cached, total, credit, elapsed_sum, ttft_sum, ttft_n, gen_ms_sum, gen_tokens)
  VALUES ($1,$2,$3,1,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
  ON CONFLICT(hour, realm, model) DO UPDATE SET
    requests=hourly.requests+1, ok=hourly.ok+excluded.ok, aborted=hourly.aborted+excluded.aborted, failed=hourly.failed+excluded.failed,
    prompt=hourly.prompt+excluded.prompt, completion=hourly.completion+excluded.completion, reasoning=hourly.reasoning+excluded.reasoning,
    cached=hourly.cached+excluded.cached, total=hourly.total+excluded.total, credit=hourly.credit+excluded.credit,
    elapsed_sum=hourly.elapsed_sum+excluded.elapsed_sum, ttft_sum=hourly.ttft_sum+excluded.ttft_sum, ttft_n=hourly.ttft_n+excluded.ttft_n,
    gen_ms_sum=hourly.gen_ms_sum+excluded.gen_ms_sum, gen_tokens=hourly.gen_tokens+excluded.gen_tokens`

func epoch(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func (d *DB) writeBatch(events []Event) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	ins, err := d.prepare(tx, insertSQL)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer ins.Close()
	st, err := d.prepareRollups(tx)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer st.close()
	st.ins = ins
	for i := range events {
		if err := st.store(&events[i]); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// stmts are the prepared statements of one write transaction.
type stmts struct {
	ins, daily, hourly *sql.Stmt
}

func (d *DB) prepareRollups(tx *sql.Tx) (*stmts, error) {
	st := &stmts{}
	dailySQL, hourlySQL := upsertDailySQL, upsertHourlySQL
	if d.driver == "pg" {
		dailySQL, hourlySQL = pgUpsertDailySQL, pgUpsertHourlySQL
	}
	var err error
	if st.daily, err = d.prepare(tx, dailySQL); err != nil {
		return nil, err
	}
	if st.hourly, err = d.prepare(tx, hourlySQL); err != nil {
		st.daily.Close()
		return nil, err
	}
	return st, nil
}

func (s *stmts) close() {
	for _, x := range []*sql.Stmt{s.ins, s.daily, s.hourly} {
		if x != nil {
			x.Close()
		}
	}
}

func (s *stmts) store(e *Event) error {
	var extra any
	if len(e.Extra) > 0 {
		extra = string(e.Extra)
	}
	if _, err := s.ins.Exec(epoch(e.At), e.Model, e.Account, e.Realm, e.KeyID, b2i(e.Stream), e.Outcome, e.Status, e.ElapsedMs,
		nullInt(e.TTFTMs), nullInt(e.GenMs), e.HeldMs, e.Prompt, e.Completion, e.Reasoning, e.Cached, e.Total, e.Credit,
		e.Effort, b2i(e.Estimated), e.Error, extra); err != nil {
		return err
	}
	var ok, aborted, failed int
	switch e.Outcome {
	case "completed", "":
		ok = 1
	case "client_aborted":
		aborted = 1
	default:
		failed = 1
	}
	var ttftSum, ttftN, genMs, genTok int64
	if e.TTFTMs != nil {
		ttftSum, ttftN = *e.TTFTMs, 1
	}
	// Estimated token counts say nothing about speed; leave them out of TPS.
	if e.GenMs != nil && *e.GenMs > 0 && !e.Estimated {
		genMs, genTok = *e.GenMs, e.Completion
	}
	vals := []any{ok, aborted, failed, e.Prompt, e.Completion, e.Reasoning, e.Cached, e.Total, e.Credit, e.ElapsedMs, ttftSum, ttftN, genMs, genTok}
	day := e.At.In(time.Local).Format("2006-01-02")
	if _, err := s.daily.Exec(append([]any{day, e.Realm, e.Model, e.Account, e.KeyID}, vals...)...); err != nil {
		return err
	}
	_, err := s.hourly.Exec(append([]any{e.At.Unix() / 3600, e.Realm, e.Model}, vals...)...)
	return err
}

var errNoDB = errors.New("usagedb: closed")
