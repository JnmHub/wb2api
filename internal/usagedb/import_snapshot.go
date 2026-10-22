package usagedb

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// importSQLiteSnapshot moves a FINAL, offline ledger snapshot atomically. Facts
// are legacy-marked in the destination; source rollups are added to both the
// live totals and rebuild baseline, so sampled/missing old details lose nothing.
// One receipt covers the whole snapshot and commits with all destination writes.
func (d *DB) importSQLiteSnapshot(ctx context.Context, src *sql.DB, path string, hasFacts bool) (int, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	outboxPath := abs + ".outbox.db"
	var configured string
	if e := src.QueryRowContext(ctx, "SELECT v FROM meta WHERE k='sqlite-outbox-path'").Scan(&configured); e == nil {
		outboxPath = configured
	} else if e != sql.ErrNoRows && hasFacts {
		return 0, e
	}
	owner, err := lockOutbox(outboxPath + ".lock")
	if err != nil {
		return 0, fmt.Errorf("usagedb: stop SQLite writer before snapshot migration: %w", err)
	}
	defer unlockOutbox(owner)
	if _, e := os.Stat(outboxPath); e == nil {
		outbox, e := sql.Open("sqlite", "file:"+outboxPath+"?mode=ro")
		if e != nil {
			return 0, e
		}
		var pending int64
		e = outbox.QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&pending)
		outbox.Close()
		if e != nil {
			return 0, e
		}
		if pending != 0 {
			return 0, errors.New("usagedb: drain SQLite outbox before snapshot migration")
		}
	} else if !os.IsNotExist(e) {
		return 0, e
	}
	source, err := src.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, err
	}
	defer source.Rollback()
	sourceID := abs
	var ledgerID string
	if e := source.QueryRowContext(ctx, "SELECT v FROM meta WHERE k='ledger-id'").Scan(&ledgerID); e == nil {
		sourceID = ledgerID
	} else if e != sql.ErrNoRows && hasFacts {
		return 0, e
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if d.driver == "pg" {
		if _, err = tx.ExecContext(ctx, "select pg_advisory_xact_lock($1)", pgUsageWriterLock); err != nil {
			return 0, err
		}
	}
	var oldCursor string
	e := tx.QueryRowContext(ctx, d.bind("SELECT v FROM meta WHERE k=?"), sqliteMetaKey(path)).Scan(&oldCursor)
	if e != nil && e != sql.ErrNoRows {
		return 0, e
	}
	if oldCursor != "" && oldCursor != "0" {
		return 0, errors.New("usagedb: source already partially row-imported; reconcile before full ledger snapshot migration")
	}
	key := "import-snapshot:" + sourceID
	var previous string
	e = tx.QueryRowContext(ctx, d.bind("SELECT v FROM meta WHERE k=?"), key).Scan(&previous)
	if e != nil && e != sql.ErrNoRows {
		return 0, e
	}
	copying := e == sql.ErrNoRows
	digest := sha256.New()
	count := 0
	scan := func(q string, store func([]string, []any) error) error {
		digest.Write([]byte(q + "\n"))
		rows, e := source.QueryContext(ctx, q)
		if e != nil {
			return e
		}
		defer rows.Close()
		cols, e := rows.Columns()
		if e != nil {
			return e
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			if e = rows.Scan(ptrs...); e != nil {
				return e
			}
			// Copy transient driver byte buffers before use and normalize JSON hashing.
			for i, v := range vals {
				if b, ok := v.([]byte); ok {
					vals[i] = string(b)
				}
			}
			b, e := json.Marshal(vals)
			if e != nil {
				return e
			}
			digest.Write(b)
			digest.Write([]byte{'\n'})
			if copying {
				if e = store(cols, vals); e != nil {
					return e
				}
			}
		}
		return rows.Err()
	}
	requestInsert, err := tx.PrepareContext(ctx, d.bind(insertSQL))
	if err != nil {
		return 0, err
	}
	defer requestInsert.Close()
	factInsert, err := tx.PrepareContext(ctx, d.bind("INSERT INTO usage_facts ("+factCols+",legacy) VALUES("+placeholders(17)+",1)"))
	if err != nil {
		return 0, err
	}
	defer factInsert.Close()
	err = scan("SELECT * FROM requests ORDER BY id", func(cols []string, vals []any) error {
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			m[c] = vals[i]
		}
		ev, _, ok := sqliteRowToEvent(m)
		if !ok {
			return errors.New("usagedb: invalid retained SQLite detail during snapshot")
		}
		if _, e := requestInsert.ExecContext(ctx, requestValues(&ev)...); e != nil {
			return e
		}
		if !hasFacts {
			if _, e := factInsert.ExecContext(ctx, factValues(&ev)...); e != nil {
				return e
			}
		}
		count++
		return nil
	})
	if err != nil {
		return 0, err
	}
	if hasFacts {
		if err = scan("SELECT "+factCols+" FROM usage_facts ORDER BY rowid", func(_ []string, vals []any) error { _, e := factInsert.ExecContext(ctx, vals...); return e }); err != nil {
			return 0, err
		}
	}
	for _, spec := range []struct{ table, dims string }{{"daily", "day,realm,model,account,key_id"}, {"hourly", "hour,realm,model"}} {
		columns := spec.dims + "," + measureNames
		// Baseline tables deliberately have no unique key: rebuild SUMs multiple
		// independent immutable source baselines, without lossy subtraction.
		baseline, e := tx.PrepareContext(ctx, d.bind("INSERT INTO legacy_"+spec.table+" ("+columns+") VALUES("+placeholders(len(strings.Split(columns, ",")))+")"))
		if e != nil {
			return 0, e
		}
		updates := []string{}
		for _, c := range strings.Split(measureNames, ",") {
			current := c
			if d.driver == "pg" {
				current = spec.table + "." + c
			}
			updates = append(updates, c+"="+current+"+excluded."+c)
		}
		live, e := tx.PrepareContext(ctx, d.bind("INSERT INTO "+spec.table+" ("+columns+") VALUES("+placeholders(len(strings.Split(columns, ",")))+") ON CONFLICT("+spec.dims+") DO UPDATE SET "+strings.Join(updates, ",")))
		if e != nil {
			baseline.Close()
			return 0, e
		}
		err = scan("SELECT "+columns+" FROM "+spec.table+" ORDER BY "+spec.dims, func(_ []string, vals []any) error {
			if _, e := baseline.ExecContext(ctx, vals...); e != nil {
				return e
			}
			_, e := live.ExecContext(ctx, vals...)
			return e
		})
		baseline.Close()
		live.Close()
		if err != nil {
			return 0, err
		}
	}
	hash := hex.EncodeToString(digest.Sum(nil))
	if !copying {
		if previous != hash {
			return 0, errors.New("usagedb: SQLite source changed after final snapshot migration; refusing stale/duplicate migration")
		}
		return 0, nil
	}
	if _, err = tx.ExecContext(ctx, d.bind("INSERT INTO meta(k,v) VALUES(?,?)"), key, hash); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }
func requestValues(e *Event) []any {
	var extra any
	if len(e.Extra) > 0 {
		extra = string(e.Extra)
	}
	return []any{epoch(e.At), e.Model, e.Account, e.Realm, e.KeyID, b2i(e.Stream), e.Outcome, e.Status, e.ElapsedMs, nullInt(e.TTFTMs), nullInt(e.GenMs), e.HeldMs, e.Prompt, e.Completion, e.Reasoning, e.Cached, e.Total, e.Credit, e.Effort, b2i(e.Estimated), e.Error, extra}
}
