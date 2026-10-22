package usagedb

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"wb2api/internal/jsonx"
	"wb2api/internal/realm"
)

// ImportProgress reports bytes read and the file size.
type ImportProgress func(done, total int64)

func metaKey(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return "import:" + abs
}

func sqliteMetaKey(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return "import-sqlite:" + abs
}

func (d *DB) getMeta(k string) (string, bool) {
	var v string
	err := d.queryRow("SELECT v FROM meta WHERE k = ?", k).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false
	}
	return v, err == nil
}

// importedFields are the legacy row fields that have a column (everything else
// is kept in the extra JSON so no information is lost).
var importedFields = map[string]bool{
	"at": true, "model": true, "account": true, "realm": true, "key": true, "stream": true, "outcome": true,
	"status": true, "elapsed_ms": true, "ttft_ms": true, "gen_ms": true, "prompt_tokens": true,
	"completion_tokens": true, "reasoning_tokens": true, "cached_tokens": true, "total_tokens": true,
	"credit": true, "reasoning_effort": true, "error": true, "message": true,
}

func asFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case float64:
		return t, true
	}
	return 0, false
}

func asInt(v any) int64 { f, _ := asFloat(v); return int64(f) }

func asStr(v any) string { s, _ := v.(string); return s }

func optInt(o *jsonx.Obj, k string) *int64 {
	v, ok := o.Lookup(k)
	if !ok || v == nil {
		return nil
	}
	if _, ok := asFloat(v); !ok {
		return nil
	}
	n := asInt(v)
	return &n
}

// rowToEvent converts one legacy usage.jsonl row.
func rowToEvent(o *jsonx.Obj) (Event, bool) {
	at, ok := asFloat(o.Get("at"))
	if !ok || at <= 0 {
		return Event{}, false
	}
	e := Event{
		At: time.Unix(0, int64(at*1e9)), Model: asStr(o.Get("model")), Account: asStr(o.Get("account")),
		Realm: asStr(o.Get("realm")), KeyID: asStr(o.Get("key")), Stream: jsonx.Truthy(o.Get("stream")),
		Outcome: asStr(o.Get("outcome")), Status: int(asInt(o.Get("status"))), ElapsedMs: asInt(o.Get("elapsed_ms")),
		TTFTMs: optInt(o, "ttft_ms"), GenMs: optInt(o, "gen_ms"),
		Prompt: asInt(o.Get("prompt_tokens")), Completion: asInt(o.Get("completion_tokens")),
		Reasoning: asInt(o.Get("reasoning_tokens")), Cached: asInt(o.Get("cached_tokens")),
		Total: asInt(o.Get("total_tokens")), Effort: asStr(o.Get("reasoning_effort")),
	}
	e.Credit, _ = asFloat(o.Get("credit"))
	if jsonx.Truthy(o.Get("error")) {
		if e.Outcome == "" {
			e.Outcome = "failed"
		}
		e.Error = asStr(o.Get("message"))
	}
	if e.Outcome == "" {
		e.Outcome = "completed"
	}
	if e.Realm == "" {
		e.Realm = realm.DetectModelRealm(e.Model, realm.Intl)
	}
	extra := jsonx.NewObj()
	o.Each(func(k string, v any) {
		if !importedFields[k] {
			extra.Set(k, v)
		}
	})
	if extra.Len() > 0 {
		if b, err := jsonx.Marshal(extra); err == nil {
			e.Extra = b
		}
	}
	return e, true
}

// ImportJSONL loads a legacy usage.jsonl. It is resumable: the byte offset
// reached is stored, so running it again only reads rows appended since (a file
// that shrank, e.g. after rotation, is read again from the start). It returns
// the number of rows imported by this call.
func (d *DB) ImportJSONL(ctx context.Context, path string, progress ImportProgress) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	var offset int64
	if v, ok := d.getMeta(metaKey(path)); ok {
		offset, _ = strconv.ParseInt(v, 10, 64)
	}
	if offset > st.Size() {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	br := bufio.NewReaderSize(f, 1<<20)
	imported := 0
	batch := make([]Event, 0, 5000)
	pos := offset
	commit := func(upTo int64) error {
		if len(batch) == 0 && upTo == offset {
			return nil
		}
		tx, err := d.beginImport()
		if err != nil {
			return err
		}
		ins, err := d.prepare(tx, insertSQL)
		if err != nil {
			tx.Rollback()
			return err
		}
		rs, err := d.prepareRollups(tx)
		if err != nil {
			ins.Close()
			tx.Rollback()
			return err
		}
		rs.ins = ins
		for i := range batch {
			if err := rs.store(&batch[i]); err != nil {
				rs.close()
				tx.Rollback()
				return err
			}
		}
		rs.close()
		if _, err := tx.Exec(d.bind("INSERT INTO meta(k, v) VALUES(?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v"),
			metaKey(path), strconv.FormatInt(upTo, 10)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		imported += len(batch)
		batch = batch[:0]
		offset = upTo
		if progress != nil {
			progress(upTo, st.Size())
		}
		return nil
	}
	for {
		if ctx.Err() != nil {
			_ = commit(pos)
			return imported, ctx.Err()
		}
		line, err := br.ReadBytes('\n')
		if err == io.EOF && len(line) > 0 && !bytes.HasSuffix(line, []byte("\n")) {
			// A partial last line (the gateway is still writing it): stop before
			// it so it is read whole next time.
			break
		}
		if len(line) > 0 {
			pos += int64(len(line))
			if t := bytes.TrimSpace(line); len(t) > 0 {
				if o, perr := jsonx.UnmarshalObj(t); perr == nil {
					if ev, ok := rowToEvent(o); ok {
						batch = append(batch, ev)
					}
				}
			}
			if len(batch) >= 5000 {
				if cerr := commit(pos); cerr != nil {
					return imported, cerr
				}
			}
		}
		if err != nil {
			break
		}
	}
	if cerr := commit(pos); cerr != nil {
		return imported, cerr
	}
	return imported, nil
}

// sqliteValue helpers accept the dynamic types returned by database/sql for a
// SQLite row: INTEGER is int64, REAL is float64 and TEXT is []byte or string.

func sqliteStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	}
	return ""
}

func sqliteFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int64:
		return float64(t), true
	case []byte:
		f, err := strconv.ParseFloat(string(t), 64)
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	}
	return 0, false
}

func sqliteInt(v any) (int64, bool) {
	f, ok := sqliteFloat(v)
	return int64(f), ok
}

func sqliteBool(v any) bool {
	if n, ok := sqliteInt(v); ok {
		return n != 0
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

func optSQLiteInt(m map[string]any, k string) *int64 {
	v, ok := m[k]
	if !ok || v == nil {
		return nil
	}
	n, ok := sqliteInt(v)
	if !ok {
		return nil
	}
	return &n
}

// sqliteRowToEvent converts one row of the old SQLite requests table. The
// column set may differ between releases, so it reads by column name instead of
// binding a fixed SELECT list.
func sqliteRowToEvent(m map[string]any) (Event, int64, bool) {
	id, _ := sqliteInt(m["id"])
	at, ok := sqliteFloat(m["at"])
	if !ok || at <= 0 {
		return Event{}, id, false
	}
	e := Event{
		At: time.Unix(0, int64(at*1e9)), Model: sqliteStr(m["model"]), Account: sqliteStr(m["account"]),
		Realm: sqliteStr(m["realm"]), KeyID: sqliteStr(m["key_id"]), Stream: sqliteBool(m["stream"]),
		Outcome: sqliteStr(m["outcome"]), Status: int(sqliteIntValue(m["status"])),
		ElapsedMs: sqliteIntValue(m["elapsed_ms"]), TTFTMs: optSQLiteInt(m, "ttft_ms"), GenMs: optSQLiteInt(m, "gen_ms"),
		HeldMs: sqliteIntValue(m["held_ms"]), Prompt: sqliteIntValue(m["prompt"]),
		Completion: sqliteIntValue(m["completion"]), Reasoning: sqliteIntValue(m["reasoning"]),
		Cached: sqliteIntValue(m["cached"]), Total: sqliteIntValue(m["total"]), Effort: sqliteStr(m["effort"]),
		Error: sqliteStr(m["error"]),
	}
	if e.KeyID == "" {
		e.KeyID = sqliteStr(m["key"])
	}
	if e.Error == "true" || e.Error == "1" {
		e.Error = sqliteStr(m["message"])
	}
	e.Credit, _ = sqliteFloat(m["credit"])
	e.Estimated = sqliteBool(m["estimated"])
	if e.Outcome == "" {
		if e.Error != "" {
			e.Outcome = "failed"
		} else {
			e.Outcome = "completed"
		}
	}
	if e.Realm == "" {
		e.Realm = realm.DetectModelRealm(e.Model, realm.Intl)
	}
	if raw := sqliteStr(m["extra"]); raw != "" && json.Valid([]byte(raw)) {
		e.Extra = json.RawMessage(raw)
	}
	return e, id, true
}

func sqliteIntValue(v any) int64 {
	n, _ := sqliteInt(v)
	return n
}

// ImportSQLite copies the request rows of a legacy SQLite usage.db into a
// PostgreSQL usage store. It is a no-op for SQLite targets and resumable: the
// highest imported request id is stored in meta in the same transaction as the
// rows, so a restart never duplicates a batch. The source is opened read-only,
// so a still-running standalone gateway can keep writing it.
func (d *DB) ImportSQLite(ctx context.Context, path string) (int, error) {
	if d.driver != "pg" {
		return 0, nil
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	legacy, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return 0, err
	}
	defer legacy.Close()
	if err := legacy.PingContext(ctx); err != nil {
		return 0, err
	}
	var probe int
	if err := legacy.QueryRowContext(ctx, "SELECT 1 FROM requests LIMIT 1").Scan(&probe); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return 0, nil
		}
		// An empty table returns sql.ErrNoRows; that is a valid legacy DB.
		if err != sql.ErrNoRows {
			return 0, err
		}
	}
	// New ledger and aggregate-only sources require an offline atomic snapshot,
	// not request-row replay (which would discard unsampled quota).
	var factsTable int
	if err := legacy.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='usage_facts'").Scan(&factsTable); err != nil {
		return 0, err
	}
	if factsTable != 0 {
		return d.importSQLiteSnapshot(ctx, legacy, path, true)
	}
	var dailyTable int
	if err := legacy.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='daily'").Scan(&dailyTable); err != nil {
		return 0, err
	}
	if dailyTable != 0 {
		var rollup, details int64
		if err := legacy.QueryRowContext(ctx, "SELECT COALESCE(SUM(requests),0) FROM daily").Scan(&rollup); err != nil {
			return 0, err
		}
		if err := legacy.QueryRowContext(ctx, "SELECT COUNT(*) FROM requests").Scan(&details); err != nil {
			return 0, err
		}
		if rollup != details {
			return d.importSQLiteSnapshot(ctx, legacy, path, false)
		}
	}
	key := sqliteMetaKey(path)
	var lastID int64
	if v, ok := d.getMeta(key); ok {
		lastID, _ = strconv.ParseInt(v, 10, 64)
	}
	imported := 0
	for {
		if err := ctx.Err(); err != nil {
			return imported, err
		}
		rows, err := legacy.QueryContext(ctx,
			"SELECT * FROM requests WHERE id > ? ORDER BY id ASC LIMIT 2000", lastID)
		if err != nil {
			return imported, err
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return imported, err
		}
		batch := make([]Event, 0, 2000)
		batchLast := lastID
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return imported, err
			}
			m := make(map[string]any, len(cols))
			for i, c := range cols {
				m[c] = vals[i]
			}
			ev, id, ok := sqliteRowToEvent(m)
			if id > batchLast {
				batchLast = id
			}
			if ok {
				batch = append(batch, ev)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return imported, err
		}
		rows.Close()
		if batchLast == lastID {
			// No new rows: record the position so an empty legacy DB is not
			// scanned on every boot, then stop.
			if err := d.setMeta(key, lastID); err != nil {
				return imported, err
			}
			return imported, nil
		}
		if err := d.importSQLiteBatch(batch, key, batchLast); err != nil {
			return imported, err
		}
		imported += len(batch)
		lastID = batchLast
		if len(batch) < 2000 {
			return imported, nil
		}
	}
}

func (d *DB) setMeta(key string, id int64) error {
	_, err := d.sql.Exec(d.bind("INSERT INTO meta(k, v) VALUES(?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v"),
		key, strconv.FormatInt(id, 10))
	return err
}

func (d *DB) importSQLiteBatch(events []Event, key string, lastID int64) error {
	tx, err := d.beginImport()
	if err != nil {
		return err
	}
	ins, err := d.prepare(tx, insertSQL)
	if err != nil {
		tx.Rollback()
		return err
	}
	st, err := d.prepareRollups(tx)
	if err != nil {
		ins.Close()
		tx.Rollback()
		return err
	}
	st.ins = ins
	defer st.close()
	for i := range events {
		if err := st.store(&events[i]); err != nil {
			tx.Rollback()
			return err
		}
	}
	if _, err := tx.Exec(d.bind("INSERT INTO meta(k, v) VALUES(?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v"),
		key, strconv.FormatInt(lastID, 10)); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
