// Package usagedb stores request usage in SQLite and keeps hourly rollups up to
// date as rows are written, so dashboards read small pre-aggregated tables
// instead of re-scanning a growing log (the legacy design: seconds to tens of
// seconds of CPU on every cold start).
//
// Record durably commits to a local FULL-sync outbox before returning. Remote
// writes are asynchronous and idempotent. Sampling drops request details only.
package usagedb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL driver for the shared usage store
	_ "modernc.org/sqlite"             // pure-Go SQLite driver (no cgo)

	platformpg "wb2api/internal/platform/pg"
)

// Event is one finished request.
type Event struct {
	At         time.Time
	localDay   string // persisted outbox day; replay must not depend on the host timezone
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
CREATE TABLE IF NOT EXISTS usage_batches (id TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS usage_sources (id TEXT PRIMARY KEY, seq BIGINT NOT NULL);
`

// OverflowPolicy controls what happens when the write queue is saturated.
type OverflowPolicy string

const (
	// OverflowDrop discards request DETAILS after QueueSize pending events.
	// Never-sampled accounting contributions still commit to the durable outbox.
	OverflowDrop OverflowPolicy = "drop"
	// OverflowSample keeps 1/N request DETAILS once 75% full. Accounting
	// facts and quota sums are NEVER sampled, including when details are omitted.
	OverflowSample OverflowPolicy = "sample"
)

// Options configures the asynchronous usage writer.
type Options struct {
	QueueSize      int            // detail-shedding threshold, NOT an accounting limit; default 8192
	Workers        int            // compatibility option; ordered durable replay currently uses 1
	Overflow       OverflowPolicy // empty means OverflowDrop
	SampleEvery    int            // keep 1/N under pressure when OverflowSample; <=0 uses 20
	WriteTimeout   time.Duration  // per transaction incl. lock wait; <=0 uses 10s
	OutboxPath     string         // stable local file, required for PG; SQLite defaults to <path>.outbox.db
	OutboxMaxBytes int64          // pending payload budget; default 256 MiB, SQLite overhead additional
	MaxEventBytes  int            // serialized event limit; default 256 KiB
	CloseTimeout   time.Duration  // final drain budget; default WriteTimeout
}

// Stats is a point-in-time view of the write pipeline.
type Stats struct {
	QueueDepth    int    `json:"queue_depth"`
	QueueCapacity int    `json:"queue_capacity"`
	Workers       int    `json:"workers"`
	Written       int64  `json:"written"`
	Dropped       int64  `json:"dropped"`
	Batches       int64  `json:"batches"`
	Failed        int64  `json:"failed"`
	Overflow      string `json:"overflow"`
	LastError     string `json:"last_error"`
	PendingRetry  int64  `json:"pending_retry"`
}

// DB is an open usage database. It speaks SQLite by default and PostgreSQL
// when opened with OpenPostgres; the schema and queries are shared.
type DB struct {
	sql    *sql.DB
	driver string // "sqlite" or "pg"

	outbox                   *sql.DB
	outboxLock               *os.File
	source                   string
	outboxBytes, outboxCount int64 // guarded by recordMu
	outboxMax                int64
	maxEventBytes            int
	closeTimeout             time.Duration
	durabilityErr            error                             // sticky until reopen; guarded by recordMu
	readiness                atomic.Pointer[readinessSnapshot] // immutable preflight; never waits behind local fsync
	writerCtx                context.Context
	writerCancel             context.CancelFunc
	flushCh                  chan chan error
	stop                     chan struct{}
	done                     chan struct{}
	once                     sync.Once
	closeErr                 error
	admissionGate            chan struct{}
	admissionQ               chan *localAdmission
	admissionClosing         chan struct{}
	admissionDone            chan struct{}
	localCommits             atomic.Int64
	closing                  atomic.Bool
	recordMu                 sync.RWMutex // serializes durable local admission, counters and Close
	closed                   bool
	writeTimeout             time.Duration
	pending                  atomic.Int64
	workers                  int
	queueCap                 int
	overflow                 OverflowPolicy
	sampleEvery              uint64
	sampleTick               atomic.Uint64

	dropped  atomic.Int64
	written  atomic.Int64
	batches  atomic.Int64
	failures atomic.Int64
	lastErr  atomic.Value // string
	sinkErr  atomic.Value // string; current sink failure, cleared after recovery
}

func normalizeOptions(o Options, driver string) Options {
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = 10 * time.Second
	}
	if o.OutboxMaxBytes <= 0 {
		o.OutboxMaxBytes = 256 << 20
	}
	if o.MaxEventBytes <= 0 {
		o.MaxEventBytes = 256 << 10
	}
	if o.CloseTimeout <= 0 {
		o.CloseTimeout = o.WriteTimeout
	}
	if o.QueueSize <= 0 {
		o.QueueSize = 8192
	}
	o.Workers = 1 // high-water receipt requires strictly ordered source replay
	if o.Overflow != OverflowSample {
		o.Overflow = OverflowDrop
	}
	if o.SampleEvery <= 0 {
		o.SampleEvery = 20
	}
	return o
}

func newDB(h *sql.DB, driver string, opts Options, sink string) (*DB, error) {
	opts = normalizeOptions(opts, driver)
	d := &DB{sql: h, driver: driver, workers: 1, queueCap: opts.QueueSize,
		overflow: opts.Overflow, sampleEvery: uint64(opts.SampleEvery),
		flushCh: make(chan chan error), writeTimeout: opts.WriteTimeout,
		stop: make(chan struct{}), done: make(chan struct{}),
		outboxMax: opts.OutboxMaxBytes, maxEventBytes: opts.MaxEventBytes, closeTimeout: opts.CloseTimeout}
	if err := d.openOutbox(opts.OutboxPath, sink); err != nil {
		h.Close()
		return nil, err
	}
	d.admissionGate = make(chan struct{}, 1)
	d.admissionQ = make(chan *localAdmission, localQueueCapacity)
	d.admissionClosing = make(chan struct{})
	d.admissionDone = make(chan struct{})
	d.writerCtx, d.writerCancel = context.WithCancel(context.Background())
	go d.localWriter()
	go d.writer()
	return d, nil
}

// Open opens (creating if needed) the database at path and starts the writer.
// It keeps the historical signature for callers that only tune the queue.
func Open(path string, queueSize int) (*DB, error) {
	return OpenWithOptions(path, Options{QueueSize: queueSize})
}

// OpenWithOptions opens SQLite with explicit writer options.
func OpenWithOptions(path string, opts Options) (*DB, error) {
	if opts.OutboxPath == "" {
		opts.OutboxPath = path + ".outbox.db"
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(0)"
	h, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	h.SetMaxOpenConns(4)
	if _, err := h.Exec(schema); err != nil {
		h.Close()
		return nil, fmt.Errorf("usagedb schema: %w", err)
	}
	if err := migrateFacts(h, "sqlite"); err != nil {
		h.Close()
		return nil, err
	}
	return newDB(h, "sqlite", opts, path)
}

// OpenPostgres opens the usage store in PostgreSQL. The tables are created on
// first use in the given database; callers normally pass the same DSN as the
// account/settings store.
func OpenPostgres(dsn string, queueSize int) (*DB, error) {
	return OpenPostgresWithOptions(dsn, Options{QueueSize: queueSize})
}

// OpenPostgresWithOptions opens PostgreSQL with explicit writer options.
func OpenPostgresWithOptions(dsn string, opts Options) (*DB, error) {
	if opts.OutboxPath == "" {
		return nil, errors.New("usagedb: PostgreSQL requires a stable local OutboxPath")
	}
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
	// each schema statement separately. Hold the shared schema advisory lock:
	// two replicas starting together otherwise race on PostgreSQL's pg_type
	// rows and one exits with a duplicate-key migration error.
	conn, err := h.Conn(ctx)
	if err != nil {
		h.Close()
		return nil, fmt.Errorf("usagedb postgres schema connection: %w", err)
	}
	keepDB := false
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(unlockCtx, "select pg_advisory_unlock($1)", platformpg.SchemaLockKey)
		_ = conn.Close()
		if !keepDB {
			h.Close()
		}
	}()
	if _, err := conn.ExecContext(ctx, "select pg_advisory_lock($1)", platformpg.SchemaLockKey); err != nil {
		return nil, fmt.Errorf("usagedb postgres schema lock: %w", err)
	}
	for _, stmt := range strings.Split(postgresSchema(), ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return nil, fmt.Errorf("usagedb postgres schema: %w", err)
		}
	}
	if err := migrateFacts(h, "pg"); err != nil {
		return nil, err
	}
	keepDB = true
	return newDB(h, "pg", opts, dsn)
}

// postgresSchema reuses the SQLite schema with the minimal syntax changes:
// BIGSERIAL for the request id and no SQLite-only WITHOUT ROWID clause.
func postgresSchema() string {
	s := strings.ReplaceAll(schema, "id          INTEGER PRIMARY KEY", "id          BIGSERIAL PRIMARY KEY")
	s = strings.ReplaceAll(s, "REAL", "DOUBLE PRECISION")
	s = strings.ReplaceAll(s, "INTEGER", "BIGINT")
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

// Dropped counts omitted request details (plus rejected calls after Close).
// Detail shedding NEVER discards accepted accounting contributions.
func (d *DB) Dropped() int64 { return d.dropped.Load() }

// Written is the number of events stored so far.
func (d *DB) Written() int64 { return d.written.Load() }

// LastError returns the last write error ("" when none).
func (d *DB) LastError() string {
	s, _ := d.lastErr.Load().(string)
	return s
}

// Stats returns the writer queue depths and counters for health panels. The
// depth is sampled without locking and may lag a concurrent enqueue by one.
func (d *DB) Stats() Stats {
	d.recordMu.RLock()
	defer d.recordMu.RUnlock()
	return Stats{QueueDepth: int(d.outboxCount), QueueCapacity: d.queueCap, Workers: d.workers,
		Written: d.written.Load(), Dropped: d.dropped.Load(), Batches: d.batches.Load(),
		Failed: d.failures.Load(), Overflow: string(d.overflow), LastError: d.LastError(), PendingRetry: d.pending.Load()}
}

// Record durably accepts a completed event locally; it never waits for remote PG.
// On error durability is not confirmed (a commit error can be ambiguous). Do not
// blindly resubmit after an ambiguous error. Local I/O failures latch Ready closed
// until reopen. Overflow/Sample affect detail only, never accounting facts.
func (d *DB) Record(e Event) error { return d.recordDurable(e) }

// Flush attempts to write everything enqueued so far. On failure it returns an
// error and retains unconfirmed batches for retry. Ignoring the return preserves
// the historical call syntax, but does not mean the data is durable.
func (d *DB) Flush() error {
	local, err := d.enqueueLocal(nil, true)
	if err != nil {
		return err
	}
	if err = <-local; err != nil {
		return err
	}
	ack := make(chan error, 1)
	select {
	case d.flushCh <- ack:
		return <-ack
	case <-d.done:
		return errors.Join(errNoDB, d.pendingError())
	}
}

// Close stops admission, drains within CloseTimeout and closes handles/ownership.
// Failure leaves unconfirmed events on disk for the next Open; it returns the
// same error on repeated calls. CloseTimeout cancels DB work, not OS fsync itself.
func (d *DB) Close() error {
	d.once.Do(func() {
		// Fence admission and start cancellation budget BEFORE waiting on any
		// producer or in-progress local fsync. Queue producers observe closing.
		timer := time.AfterFunc(d.closeTimeout, d.writerCancel)
		d.closing.Store(true)
		close(d.admissionClosing)
		d.admissionGate <- struct{}{}
		close(d.admissionQ)
		<-d.admissionGate
		<-d.admissionDone
		d.recordMu.Lock()
		d.closed = true
		d.publishReadinessLocked()
		close(d.stop)
		d.recordMu.Unlock()
		<-d.done
		timer.Stop()
		d.writerCancel()
		d.closeErr = errors.Join(d.durabilityErr, d.pendingError(), d.outbox.Close(), unlockOutbox(d.outboxLock), d.sql.Close())
	})
	return d.closeErr
}

const (
	batchMax   = 256
	batchDelay = 150 * time.Millisecond

	// pgUsageWriterLock serialises PostgreSQL batch transactions across writer
	// goroutines and replicas (ASCII "wb2api-u").
	pgUsageWriterLock int64 = 0x7762326170692d75
)

func (d *DB) writer() {
	defer close(d.done)
	timer := time.NewTicker(batchDelay)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			_ = d.drainOutbox(false)
		case ack := <-d.flushCh:
			ack <- d.drainOutbox(true)
		case <-d.stop:
			_ = d.drainOutbox(true)
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

func (d *DB) writeBatch(ctx context.Context, unit writeUnit) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Retain cross-replica lock until ordered rollup batching is proven safe.
	if d.driver == "pg" {
		if _, err = tx.ExecContext(ctx, "select pg_advisory_xact_lock($1)", pgUsageWriterLock); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, d.bind("INSERT INTO usage_sources(id,seq) VALUES(?,0) ON CONFLICT(id) DO NOTHING"), unit.id); err != nil {
		return err
	}
	var through int64
	if err = tx.QueryRowContext(ctx, d.bind("SELECT seq FROM usage_sources WHERE id=?"), unit.id).Scan(&through); err != nil {
		return err
	}
	if d.driver == "pg" {
		through, err = d.writePGBulk(ctx, tx, unit, through)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE usage_sources SET seq=$1 WHERE id=$2", through, unit.id); err != nil {
			return err
		}
		return tx.Commit()
	}
	ins, err := tx.PrepareContext(ctx, d.bind(insertSQL))
	if err != nil {
		return err
	}
	defer ins.Close()
	st, err := d.prepareRollupsContext(ctx, tx)
	if err != nil {
		return err
	}
	defer st.close()
	st.ins = ins
	for i := range unit.events {
		if unit.seqs[i] <= through {
			continue
		}
		if err = st.storeDetailContext(ctx, &unit.events[i], unit.details[i]); err != nil {
			return err
		}
		through = unit.seqs[i]
	}
	if _, err = tx.ExecContext(ctx, d.bind("UPDATE usage_sources SET seq=? WHERE id=?"), through, unit.id); err != nil {
		return err
	}
	return tx.Commit()
}

// stmts are the prepared statements of one write transaction.
type stmts struct {
	ins, facts, daily, hourly *sql.Stmt
}

func (d *DB) prepareRollups(tx *sql.Tx) (*stmts, error) {
	return d.prepareRollupsContext(context.Background(), tx)
}

func (d *DB) prepareRollupsContext(ctx context.Context, tx *sql.Tx) (*stmts, error) {
	st := &stmts{}
	var ferr error
	st.facts, ferr = tx.PrepareContext(ctx, d.bind(insertFactsSQL))
	if ferr != nil {
		return nil, ferr
	}
	dailySQL, hourlySQL := upsertDailySQL, upsertHourlySQL
	if d.driver == "pg" {
		dailySQL, hourlySQL = pgUpsertDailySQL, pgUpsertHourlySQL
	}
	var err error
	if st.daily, err = tx.PrepareContext(ctx, d.bind(dailySQL)); err != nil {
		st.close()
		return nil, err
	}
	if st.hourly, err = tx.PrepareContext(ctx, d.bind(hourlySQL)); err != nil {
		st.close()
		return nil, err
	}
	return st, nil
}

func (s *stmts) close() {
	for _, x := range []*sql.Stmt{s.ins, s.facts, s.daily, s.hourly} {
		if x != nil {
			x.Close()
		}
	}
}

func (s *stmts) store(e *Event) error {
	return s.storeContext(context.Background(), e)
}

func (s *stmts) storeContext(ctx context.Context, e *Event) error {
	return s.storeDetailContext(ctx, e, true)
}

func (s *stmts) storeDetailContext(ctx context.Context, e *Event, detail bool) error {
	var extra any
	if len(e.Extra) > 0 {
		extra = string(e.Extra)
	}
	if detail {
		if _, err := s.ins.ExecContext(ctx, epoch(e.At), e.Model, e.Account, e.Realm, e.KeyID, b2i(e.Stream), e.Outcome, e.Status, e.ElapsedMs,
			nullInt(e.TTFTMs), nullInt(e.GenMs), e.HeldMs, e.Prompt, e.Completion, e.Reasoning, e.Cached, e.Total, e.Credit,
			e.Effort, b2i(e.Estimated), e.Error, extra); err != nil {
			return err
		}
	}
	if s.facts != nil {
		if _, err := s.facts.ExecContext(ctx, factValues(e)...); err != nil {
			return err
		}
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
	day := eventDay(e)
	if _, err := s.daily.ExecContext(ctx, append([]any{day, e.Realm, e.Model, e.Account, e.KeyID}, vals...)...); err != nil {
		return err
	}
	_, err := s.hourly.ExecContext(ctx, append([]any{e.At.Unix() / 3600, e.Realm, e.Model}, vals...)...)
	return err
}

var errNoDB = errors.New("usagedb: closed")
