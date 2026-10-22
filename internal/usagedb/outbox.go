package usagedb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

var ErrOutboxFull = errors.New("usagedb: durable outbox payload budget exhausted")
var ErrEventTooLarge = errors.New("usagedb: event exceeds durable payload limit")

// Health distinguishes local durability from remote delivery. Remote failure
// does not reject local durable recording until the outbox budget is exhausted.
// Ready is a preflight, NOT a reservation; another completion can fill disk
// after it returns. Callers MUST also handle Record's error after output.
type Health struct {
	Ready           bool   `json:"ready"`
	PendingEvents   int64  `json:"pending_events"`
	PendingBytes    int64  `json:"pending_bytes"`
	MaxBytes        int64  `json:"max_bytes"`
	DurabilityError string `json:"durability_error"`
	SinkError       string `json:"sink_error"`
}

type readinessSnapshot struct {
	err    error
	health Health
}

// Ready reads the last published admission state without SQL or recordMu, so a
// preceding completion's FULL fsync cannot delay a new request's preflight.
// It is still NOT a reservation: in-progress admissions may consume capacity.
func (d *DB) Ready() error {
	if d.closing.Load() {
		return errNoDB
	}
	s := d.readiness.Load()
	if s == nil {
		return errNoDB
	}
	return s.err
}

// publishReadinessLocked runs under recordMu exclusively, or during startup
// before DB publication. Each snapshot is immutable and published only after a
// complete local state transition. Sticky failures/Close cannot become Ready.
func (d *DB) publishReadinessLocked() {
	err := d.readyLocked()
	h := Health{Ready: err == nil, PendingEvents: d.outboxCount, PendingBytes: d.outboxBytes, MaxBytes: d.outboxMax}
	if d.durabilityErr != nil {
		h.DurabilityError = d.durabilityErr.Error()
	}
	d.readiness.Store(&readinessSnapshot{err: err, health: h})
}
func (d *DB) readyLocked() error {
	if d.closed || d.closing.Load() {
		return errNoDB
	}
	if d.durabilityErr != nil {
		return d.durabilityErr
	}
	// Leave conservative headroom; tiny explicitly configured outboxes remain
	// usable, but this is still not an in-flight request capacity reservation.
	headroom := min(int64(d.maxEventBytes), d.outboxMax/2)
	if d.outboxBytes >= d.outboxMax-headroom {
		return ErrOutboxFull
	}
	return nil
}
func (d *DB) Health() Health {
	s := d.readiness.Load()
	h := Health{}
	if s != nil {
		h = s.health
	}
	if d.closing.Load() {
		h.Ready = false
	}
	h.SinkError, _ = d.sinkErr.Load().(string)
	return h
}
func (d *DB) localFailure(err error) error {
	d.recordMu.Lock()
	defer d.recordMu.Unlock()
	return d.localFailureLocked(err)
}
func (d *DB) localFailureLocked(err error) error {
	d.failures.Add(1)
	d.lastErr.Store(err.Error())
	d.durabilityErr = fmt.Errorf("usagedb: local durability unavailable (reopen required): %w", err)
	d.publishReadinessLocked()
	return d.durabilityErr
}

func (d *DB) openOutbox(path, sink string) (retErr error) {
	if path == "" || path == ":memory:" || strings.Contains(path, "?") {
		return errors.New("usagedb: OutboxPath must be a local filesystem path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if d.driver == "sqlite" {
		a, _ := filepath.Abs(sink)
		if strings.EqualFold(a, abs) {
			return errors.New("usagedb: outbox and sink must be different files")
		}
	}
	lock, err := lockOutbox(abs + ".lock")
	if err != nil {
		return fmt.Errorf("usagedb outbox ownership: %w", err)
	}
	d.outboxLock = lock
	defer func() {
		if retErr != nil {
			if d.outbox != nil {
				d.outbox.Close()
			}
			_ = unlockOutbox(lock)
		}
	}()
	// URL escaping preserves paths containing '?'/'#' and Windows drive letters.
	uriPath := filepath.ToSlash(abs)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	u := url.URL{Scheme: "file", Path: uriPath}
	h, err := sql.Open("sqlite", u.String()+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=wal_autocheckpoint(128)")
	if err != nil {
		return err
	}
	d.outbox = h
	h.SetMaxOpenConns(1)
	_, err = h.Exec(`CREATE TABLE IF NOT EXISTS outbox_meta(k TEXT PRIMARY KEY,v TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS events(seq INTEGER PRIMARY KEY AUTOINCREMENT,payload BLOB NOT NULL,detail INTEGER NOT NULL);
 INSERT INTO outbox_meta(k,v) VALUES('source','` + rand.Text() + `') ON CONFLICT(k) DO NOTHING;`)
	if err != nil {
		return err
	}
	var sinkID string
	if err = d.queryRow("SELECT v FROM meta WHERE k='ledger-id'").Scan(&sinkID); err != nil {
		return err
	}
	var existing string
	err = h.QueryRow("SELECT v FROM outbox_meta WHERE k='sink'").Scan(&existing)
	if err == sql.ErrNoRows {
		_, err = h.Exec("INSERT INTO outbox_meta(k,v) VALUES('sink',?)", sinkID)
	} else if err == nil && existing != sinkID {
		return errors.New("usagedb: outbox belongs to a different sink ledger; refusing replay")
	}
	if err != nil {
		return err
	}
	if err = h.QueryRow("SELECT v FROM outbox_meta WHERE k='source'").Scan(&d.source); err != nil {
		return err
	}
	if err = h.QueryRow("SELECT COUNT(*),COALESCE(SUM(length(payload)),0) FROM events").Scan(&d.outboxCount, &d.outboxBytes); err != nil {
		return err
	}
	if d.driver == "sqlite" {
		var old string
		e := d.queryRow("SELECT v FROM meta WHERE k='sqlite-outbox-path'").Scan(&old)
		if e == nil && !strings.EqualFold(old, abs) {
			return errors.New("usagedb: SQLite ledger outbox path changed; drain/migrate original source explicitly")
		}
		if e != nil && e != sql.ErrNoRows {
			return e
		}
		if _, err = d.sql.Exec("INSERT INTO meta(k,v) VALUES('sqlite-outbox-path',?) ON CONFLICT(k) DO NOTHING", abs); err != nil {
			return err
		}
	}
	d.pending.Store(d.outboxCount)
	d.publishReadinessLocked()
	return nil
}

func (d *DB) recordDurable(e Event) error {
	ack, err := d.enqueueLocal(&e, false)
	if err != nil {
		return err
	}
	return <-ack
}

type durableEvent struct {
	Event Event
	Day   string
}

func (d *DB) loadUnit(ctx context.Context, until int64) (writeUnit, error) {
	u := writeUnit{id: d.source}
	rows, err := d.outbox.QueryContext(ctx, "SELECT seq,payload,detail FROM events WHERE seq<=? ORDER BY seq LIMIT ?", until, batchMax)
	if err != nil {
		return u, err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var payload []byte
		var detail int
		if err = rows.Scan(&seq, &payload, &detail); err != nil {
			return u, err
		}
		var e durableEvent
		if err = json.Unmarshal(payload, &e); err != nil {
			return u, fmt.Errorf("outbox corrupt event %d: %w", seq, err)
		}
		e.Event.localDay = e.Day
		u.seqs = append(u.seqs, seq)
		u.events = append(u.events, e.Event)
		u.details = append(u.details, detail != 0)
		u.bytes += int64(len(payload))
		// Bound replay decoding/retained memory even when diagnostics approach the
		// configured per-event limit. At most one extra row above this byte budget.
		if u.bytes >= 4<<20 {
			break
		}
	}
	return u, rows.Err()
}

// Create the lock file once and retain it. Removing it after unlock can race a
// third opener and permit two owners to lock different file identities.
func openLockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
}
