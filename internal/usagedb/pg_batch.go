package usagedb

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
)

// writePGBulk keeps the source receipt transaction/advisory lock, but reduces
// <=256 events to at most four data statements: details, facts, daily, hourly.
// Rollup keys are unique and sorted before INSERT, so a single INSERT never
// updates one target twice and row locks follow deterministic dimension order.
func (d *DB) writePGBulk(ctx context.Context, tx *sql.Tx, u writeUnit, through int64) (int64, error) {
	details := make([][]any, 0, len(u.events))
	facts := make([][]any, 0, len(u.events))
	type dailyKey struct{ day, realm, model, account, key string }
	type hourlyKey struct {
		hour         int64
		realm, model string
	}
	daily := map[dailyKey]*rollupSums{}
	hourly := map[hourlyKey]*rollupSums{}
	for i := range u.events {
		if u.seqs[i] <= through {
			continue
		}
		e := &u.events[i]
		facts = append(facts, factValues(e))
		if u.details[i] {
			details = append(details, requestValues(e))
		}
		dk := dailyKey{eventDay(e), e.Realm, e.Model, e.Account, e.KeyID}
		hk := hourlyKey{e.At.Unix() / 3600, e.Realm, e.Model}
		if daily[dk] == nil {
			daily[dk] = &rollupSums{}
		}
		if hourly[hk] == nil {
			hourly[hk] = &rollupSums{}
		}
		if err := daily[dk].add(e); err != nil {
			return through, err
		}
		if err := hourly[hk].add(e); err != nil {
			return through, err
		}
		through = u.seqs[i]
	}
	if len(facts) == 0 {
		return through, nil
	}
	// Reuse the authoritative request column list, avoiding drift with import and
	// SQLite path as fields evolve. Only the VALUES fragment is replaced.
	requestColumns := strings.TrimSpace(strings.SplitN(strings.SplitN(insertSQL, "(", 2)[1], ")", 2)[0])
	if err := d.pgMultiInsert(ctx, tx, "requests", requestColumns, details, ""); err != nil {
		return through, err
	}
	if err := d.pgMultiInsert(ctx, tx, "usage_facts", factCols, facts, ""); err != nil {
		return through, err
	}
	dkeys := make([]dailyKey, 0, len(daily))
	for key := range daily {
		dkeys = append(dkeys, key)
	}
	sort.Slice(dkeys, func(i, j int) bool {
		a, b := dkeys[i], dkeys[j]
		av, bv := [5]string{a.day, a.realm, a.model, a.account, a.key}, [5]string{b.day, b.realm, b.model, b.account, b.key}
		for k := range av {
			if av[k] != bv[k] {
				return av[k] < bv[k]
			}
		}
		return false
	})
	drows := make([][]any, 0, len(dkeys))
	for _, k := range dkeys {
		drows = append(drows, append([]any{k.day, k.realm, k.model, k.account, k.key}, daily[k].values()...))
	}
	if err := d.pgMultiInsert(ctx, tx, "daily", "day,realm,model,account,key_id,"+measureNames, drows, rollupConflict("daily", "day,realm,model,account,key_id")); err != nil {
		return through, err
	}
	hkeys := make([]hourlyKey, 0, len(hourly))
	for key := range hourly {
		hkeys = append(hkeys, key)
	}
	sort.Slice(hkeys, func(i, j int) bool {
		a, b := hkeys[i], hkeys[j]
		if a.hour != b.hour {
			return a.hour < b.hour
		}
		if a.realm != b.realm {
			return a.realm < b.realm
		}
		return a.model < b.model
	})
	hrows := make([][]any, 0, len(hkeys))
	for _, k := range hkeys {
		hrows = append(hrows, append([]any{k.hour, k.realm, k.model}, hourly[k].values()...))
	}
	if err := d.pgMultiInsert(ctx, tx, "hourly", "hour,realm,model,"+measureNames, hrows, rollupConflict("hourly", "hour,realm,model")); err != nil {
		return through, err
	}
	return through, nil
}

func rollupConflict(table, dimensions string) string {
	updates := make([]string, 0, 15)
	for _, col := range strings.Split(measureNames, ",") {
		updates = append(updates, col+"="+table+"."+col+"+excluded."+col)
	}
	return " ON CONFLICT(" + dimensions + ") DO UPDATE SET " + strings.Join(updates, ",")
}
func (d *DB) pgMultiInsert(ctx context.Context, tx *sql.Tx, table, columns string, rows [][]any, suffix string) error {
	if len(rows) == 0 {
		return nil
	}
	width := len(rows[0])
	if len(rows) > batchMax || width*len(rows) > 65535 {
		return fmt.Errorf("usagedb: PG batch exceeds parameter bound")
	}
	args := make([]any, 0, len(rows)*width)
	tuples := make([]string, 0, len(rows))
	for _, row := range rows {
		if len(row) != width {
			return fmt.Errorf("usagedb: PG batch row width mismatch")
		}
		args = append(args, row...)
		tuples = append(tuples, "("+placeholders(width)+")")
	}
	_, err := tx.ExecContext(ctx, d.bind("INSERT INTO "+table+" ("+columns+") VALUES "+strings.Join(tuples, ",")+suffix), args...)
	return err
}

type rollupSums struct {
	n      [15]int64
	credit float64
}

func (s *rollupSums) add(e *Event) error {
	n := [15]int64{1, 0, 0, 0, e.Prompt, e.Completion, e.Reasoning, e.Cached, e.Total, 0, e.ElapsedMs, 0, 0, 0, 0}
	switch e.Outcome {
	case "completed", "":
		n[1] = 1
	case "client_aborted":
		n[2] = 1
	default:
		n[3] = 1
	}
	if e.TTFTMs != nil {
		n[11] = *e.TTFTMs
		n[12] = 1
	}
	if e.GenMs != nil && *e.GenMs > 0 && !e.Estimated {
		n[13] = *e.GenMs
		n[14] = e.Completion
	}
	for i, v := range n {
		if (v > 0 && s.n[i] > math.MaxInt64-v) || (v < 0 && s.n[i] < math.MinInt64-v) {
			return fmt.Errorf("usagedb: rollup BIGINT overflow")
		}
		s.n[i] += v
	}
	s.credit += e.Credit
	if math.IsInf(s.credit, 0) || math.IsNaN(s.credit) {
		return fmt.Errorf("usagedb: rollup credit overflow")
	}
	return nil
}
func (s *rollupSums) values() []any {
	out := make([]any, len(s.n))
	for i, v := range s.n {
		out[i] = v
	}
	out[9] = s.credit
	return out
}
