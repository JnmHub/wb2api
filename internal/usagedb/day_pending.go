package usagedb

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// DayIncludingPending returns consistent delivered sink totals plus this
// process's durable pending contributions not present in that sink snapshot.
// Intended for Today startup seeding; does NOT see other replicas' outboxes.
// Error means the sink snapshot/local queue could not be read, not zero usage.
func (d *DB) DayIncludingPending(start time.Time) (DayCounters, error) {
	out := DayCounters{RequestsByAccount: map[string]int64{}, TokensByAccount: map[string]int64{}, CreditsByAccount: map[string]float64{}, TokensByAccountModel: map[string]map[string]int64{}}
	add := func(account, model string, tokens, requests int64, credit float64) {
		if account == "" {
			return
		}
		out.RequestsByAccount[account] += requests
		out.TokensByAccount[account] += tokens
		out.CreditsByAccount[account] += credit
		if out.TokensByAccountModel[account] == nil {
			out.TokensByAccountModel[account] = map[string]int64{}
		}
		out.TokensByAccountModel[account][model] += tokens
	}
	d.recordMu.RLock()
	defer d.recordMu.RUnlock()
	if d.closed {
		return out, errNoDB
	}
	ctx, cancel := context.WithTimeout(context.Background(), d.writeTimeout)
	defer cancel()
	tx, err := d.sql.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	// Reading the receipt first establishes the transaction snapshot. A concurrent
	// sink commit is wholly before or after it; local ack deletion holds recordMu
	// exclusively, so every event newer than this snapshot remains available.
	var through int64
	err = tx.QueryRowContext(ctx, d.bind("SELECT seq FROM usage_sources WHERE id=?"), d.source).Scan(&through)
	if err != nil && err != sql.ErrNoRows {
		return out, err
	}
	q := `SELECT account,model,SUM(total),SUM(credit),SUM(requests) FROM daily WHERE day>=? AND account<>'' GROUP BY account,model`
	arg := any(localDay(start))
	if !isMidnight(start) {
		q = `SELECT account,model,SUM(total),SUM(credit),COUNT(*) FROM usage_facts WHERE at>=? AND account<>'' GROUP BY account,model`
		arg = epoch(start)
	}
	rows, err := tx.QueryContext(ctx, d.bind(q), arg)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var account, model string
		var tok, req int64
		var credit float64
		if err = rows.Scan(&account, &model, &tok, &credit, &req); err != nil {
			rows.Close()
			return out, err
		}
		add(account, model, tok, req, credit)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	rows, err = d.outbox.QueryContext(ctx, "SELECT payload FROM events WHERE seq>? ORDER BY seq", through)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var payload []byte
		if err = rows.Scan(&payload); err != nil {
			return out, err
		}
		var ev durableEvent
		if err = json.Unmarshal(payload, &ev); err != nil {
			return out, err
		}
		include := !ev.Event.At.Before(start)
		// Match Day's legacy daily semantics when start is local midnight. Stored
		// day preserves the original host day even after timezone changes/replay.
		if isMidnight(start) {
			include = ev.Day >= localDay(start)
		}
		if include {
			add(ev.Event.Account, ev.Event.Model, ev.Event.Total, 1, ev.Event.Credit)
		}
	}
	return out, rows.Err()
}
