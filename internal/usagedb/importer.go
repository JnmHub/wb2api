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

func (d *DB) getMeta(k string) (string, bool) {
	var v string
	err := d.sql.QueryRow("SELECT v FROM meta WHERE k = ?", k).Scan(&v)
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
		tx, err := d.sql.Begin()
		if err != nil {
			return err
		}
		ins, err := tx.Prepare(insertSQL)
		if err != nil {
			tx.Rollback()
			return err
		}
		rs, err := prepareRollups(tx)
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
		if _, err := tx.Exec("INSERT INTO meta(k, v) VALUES(?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v",
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
