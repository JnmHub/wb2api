package usagedb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"strings"
)

// usage_facts is the immutable, NEVER sampled accounting ledger. It excludes
// request diagnostics/payloads, but keeps the dimensions/time needed for exact
// quota and rolling-window queries and a lossless rollup rebuild. Sink history
// has no automatic retention limit; operators must plan/monitor disk capacity.
const factsSchema = `CREATE TABLE IF NOT EXISTS usage_facts (
 at REAL NOT NULL, day TEXT NOT NULL, model TEXT NOT NULL, account TEXT NOT NULL,
 realm TEXT NOT NULL, key_id TEXT NOT NULL, outcome TEXT NOT NULL,
 elapsed_ms BIGINT NOT NULL, ttft_ms BIGINT, gen_ms BIGINT,
 prompt BIGINT NOT NULL, completion BIGINT NOT NULL, reasoning BIGINT NOT NULL,
 cached BIGINT NOT NULL, total BIGINT NOT NULL, credit REAL NOT NULL, estimated INTEGER NOT NULL, legacy INTEGER NOT NULL DEFAULT 0);
 CREATE INDEX IF NOT EXISTS usage_facts_at ON usage_facts(at);
 CREATE INDEX IF NOT EXISTS usage_facts_account_at ON usage_facts(account,at);`
const factCols = `at,day,model,account,realm,key_id,outcome,elapsed_ms,ttft_ms,gen_ms,prompt,completion,reasoning,cached,total,credit,estimated`
const insertFactsSQL = `INSERT INTO usage_facts (` + factCols + `) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

func eventDay(e *Event) string {
	if e.localDay != "" {
		return e.localDay
	}
	return localDay(e.At)
}
func factValues(e *Event) []any {
	return []any{epoch(e.At), eventDay(e), e.Model, e.Account, e.Realm, e.KeyID, e.Outcome, e.ElapsedMs, nullInt(e.TTFTMs), nullInt(e.GenMs), e.Prompt, e.Completion, e.Reasoning, e.Cached, e.Total, e.Credit, b2i(e.Estimated)}
}

func migrateFacts(h *sql.DB, driver string) error {
	d := &DB{sql: h, driver: driver}
	tx, err := h.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if driver == "pg" {
		if _, err = tx.Exec("select pg_advisory_xact_lock($1)", pgUsageWriterLock); err != nil {
			return err
		}
	}
	schema := factsSchema
	if driver == "pg" {
		schema = strings.ReplaceAll(schema, "REAL", "DOUBLE PRECISION")
	}
	for _, s := range strings.Split(schema, ";") {
		if strings.TrimSpace(s) != "" {
			if _, err = tx.Exec(s); err != nil {
				return err
			}
		}
	}
	if driver == "pg" {
		var widened string
		e := tx.QueryRow("SELECT v FROM meta WHERE k='usage-bigint-v1'").Scan(&widened)
		if e != nil && e != sql.ErrNoRows {
			return e
		}
		if e == sql.ErrNoRows {
			for _, spec := range []struct{ table, cols string }{
				{"requests", "elapsed_ms,ttft_ms,gen_ms,held_ms,prompt,completion,reasoning,cached,total"},
				{"daily", strings.ReplaceAll(measureNames, ",credit", "")},
				{"hourly", "hour," + strings.ReplaceAll(measureNames, ",credit", "")},
			} {
				clauses := []string{}
				for _, col := range strings.Split(spec.cols, ",") {
					clauses = append(clauses, "ALTER COLUMN "+col+" TYPE BIGINT")
				}
				if _, err = tx.Exec("ALTER TABLE " + spec.table + " " + strings.Join(clauses, ",")); err != nil {
					return err
				}
			}
			if _, err = tx.Exec("INSERT INTO meta(k,v) VALUES('usage-bigint-v1','1')"); err != nil {
				return err
			}
		}
	}
	var v string
	err = tx.QueryRow("SELECT v FROM meta WHERE k='facts-v1'").Scan(&v)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == sql.ErrNoRows {
		// Existing rollups are left intact: earlier loss/sampling cannot be repaired
		// retroactively. Track their difference from details as a legacy baseline so
		// rebuilding cannot erase even pre-upgrade aggregate-only contributions.
		if _, err = tx.Exec(`CREATE TABLE IF NOT EXISTS legacy_daily AS SELECT * FROM daily`); err != nil {
			return err
		}
		if _, err = tx.Exec(`CREATE TABLE IF NOT EXISTS legacy_hourly AS SELECT * FROM hourly`); err != nil {
			return err
		}
		// Snapshot includes every pre-upgrade row. Rebuild adds only post-upgrade
		// facts to this baseline; exact queries still include all historical facts.
		if _, err = tx.Exec("INSERT INTO usage_facts (" + factCols + ",legacy) SELECT at," + d.requestDayExpr() + ",model,account,realm,key_id,outcome,elapsed_ms,ttft_ms,gen_ms,prompt,completion,reasoning,cached,total,credit,estimated,1 FROM requests"); err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO meta(k,v) VALUES('facts-v1','1')"); err != nil {
			return err
		}
		// Legacy facts are marked legacy=1; post-migration facts default to 0.
	}
	if _, err = tx.Exec(d.bind("INSERT INTO meta(k,v) VALUES('ledger-id',?) ON CONFLICT(k) DO NOTHING"), rand.Text()); err != nil {
		return err
	}
	return tx.Commit()
}

const rebuildMeasures = `COUNT(*),
 SUM(CASE WHEN outcome IN ('completed','') THEN 1 ELSE 0 END),
 SUM(CASE WHEN outcome='client_aborted' THEN 1 ELSE 0 END),
 SUM(CASE WHEN outcome NOT IN ('completed','','client_aborted') THEN 1 ELSE 0 END),
 SUM(prompt),SUM(completion),SUM(reasoning),SUM(cached),SUM(total),SUM(credit),SUM(elapsed_ms),SUM(COALESCE(ttft_ms,0)),
 SUM(CASE WHEN ttft_ms IS NOT NULL THEN 1 ELSE 0 END),SUM(CASE WHEN gen_ms>0 AND estimated=0 THEN gen_ms ELSE 0 END),
 SUM(CASE WHEN gen_ms>0 AND estimated=0 THEN completion ELSE 0 END)`
const measureNames = `requests,ok,aborted,failed,prompt,completion,reasoning,cached,total,credit,elapsed_sum,ttft_sum,ttft_n,gen_ms_sum,gen_tokens`

// RebuildRollups reconstructs from the never-sampled ledger plus the original
// legacy rollup baseline. It NEVER rebuilds from sampled requests. Must not run
// concurrently with external SQL writers; normal outbox/import writers use the
// same transaction lock (SQLite writer lock / PG advisory lock).
func (d *DB) RebuildRollups(ctx context.Context) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if d.driver == "pg" {
		if _, err = tx.ExecContext(ctx, "select pg_advisory_xact_lock($1)", pgUsageWriterLock); err != nil {
			return err
		}
	}
	hour := "CAST(at / 3600 AS INTEGER)"
	if d.driver == "pg" {
		hour = "CAST(FLOOR(at / 3600) AS BIGINT)"
	}
	// Legacy snapshots retain aggregate-only historical contributions. New facts
	// are marked legacy=0, so rebuild never counts the old details twice.
	for _, spec := range []struct{ table, legacy, dimensions, selectDims string }{
		{"daily", "legacy_daily", "day,realm,model,account,key_id", "day,realm,model,account,key_id"},
		{"hourly", "legacy_hourly", "hour,realm,model", hour + " AS hour,realm,model"},
	} {
		if _, err = tx.ExecContext(ctx, "DELETE FROM "+spec.table); err != nil {
			return err
		}
		sums := []string{}
		for _, col := range strings.Split(measureNames, ",") {
			sums = append(sums, "SUM("+col+")")
		}
		columns := spec.dimensions + "," + measureNames
		q := "INSERT INTO " + spec.table + " (" + columns + ") SELECT " + spec.dimensions + "," + strings.Join(sums, ",") + " FROM (" +
			"SELECT " + columns + " FROM " + spec.legacy + " UNION ALL " +
			"SELECT " + spec.selectDims + "," + rebuildMeasures + " FROM usage_facts WHERE legacy=0 GROUP BY " + spec.dimensions + ") AS contributions GROUP BY " + spec.dimensions
		if _, err = tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("rebuild %s: %w", spec.table, err)
		}
	}
	return tx.Commit()
}
