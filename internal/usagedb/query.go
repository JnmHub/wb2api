package usagedb

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Filter narrows a query. Zero values mean "no limit".
type Filter struct {
	Since, Until time.Time
	Realm        string
	Model        string
	Account      string
	KeyID        string
}

// Agg is an aggregate over a set of requests.
type Agg struct {
	Requests   int64
	OK         int64
	Aborted    int64
	Failed     int64
	Prompt     int64
	Completion int64
	Reasoning  int64
	Cached     int64
	Total      int64
	Credit     float64

	AvgElapsedMs float64 // over all requests
	AvgTTFTMs    float64 // over requests that reported a first-token time
	// TPS is completion tokens per second of generation time, summed over the
	// requests that reported one (a token-weighted rate, not a mean of rates).
	TPS float64
	// CacheHitPct is cached prompt tokens as a share of all prompt tokens.
	CacheHitPct float64
}

// Column lists producing the 15 sums scanAgg expects, per source table.
const rollupCols = `SUM(requests), SUM(ok), SUM(aborted), SUM(failed), SUM(prompt), SUM(completion), SUM(reasoning), SUM(cached),
  SUM(total), SUM(credit), SUM(elapsed_sum), SUM(ttft_sum), SUM(ttft_n), SUM(gen_ms_sum), SUM(gen_tokens)`

const requestCols = `COUNT(*),
  SUM(CASE WHEN outcome IN ('completed','') THEN 1 ELSE 0 END),
  SUM(CASE WHEN outcome = 'client_aborted' THEN 1 ELSE 0 END),
  SUM(CASE WHEN outcome NOT IN ('completed','','client_aborted') THEN 1 ELSE 0 END),
  SUM(prompt), SUM(completion), SUM(reasoning), SUM(cached),
  SUM(total), SUM(credit), SUM(elapsed_ms), SUM(COALESCE(ttft_ms,0)),
  SUM(CASE WHEN ttft_ms IS NOT NULL THEN 1 ELSE 0 END),
  SUM(CASE WHEN gen_ms > 0 AND estimated = 0 THEN gen_ms ELSE 0 END),
  SUM(CASE WHEN gen_ms > 0 AND estimated = 0 THEN completion ELSE 0 END)`

// source says which table answers a query and how to filter it.
type source struct {
	cols   string // aggregate column list
	from   string
	where  string
	args   []any
	timeEx string // SQL expression of the row's local time, for bucketing
}

func localDay(t time.Time) string { return t.In(time.Local).Format("2006-01-02") }

func isMidnight(t time.Time) bool {
	l := t.In(time.Local)
	return l.Hour() == 0 && l.Minute() == 0 && l.Second() == 0 && l.Nanosecond() == 0
}

// aligned reports whether the time range ends on local-day boundaries, i.e. the
// daily rollup answers it exactly.
func (f Filter) aligned() bool {
	return (f.Since.IsZero() || isMidnight(f.Since)) && (f.Until.IsZero() || isMidnight(f.Until))
}

func and(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

// dimConds adds the equality filters; account/key are not columns of hourly.
func (f Filter) dimConds(conds []string, args []any, withAccountKey bool) ([]string, []any) {
	add := func(col, v string) {
		if v != "" {
			conds = append(conds, col+" = ?")
			args = append(args, v)
		}
	}
	add("realm", f.Realm)
	add("model", f.Model)
	if withAccountKey {
		add("account", f.Account)
		add("key_id", f.KeyID)
	}
	return conds, args
}

// pickDaily answers from the daily rollup (exact for day-aligned ranges).
func (f Filter) pickDaily() source {
	var conds []string
	var args []any
	if !f.Since.IsZero() {
		conds = append(conds, "day >= ?")
		args = append(args, localDay(f.Since))
	}
	if !f.Until.IsZero() {
		conds = append(conds, "day < ?")
		args = append(args, localDay(f.Until))
	}
	conds, args = f.dimConds(conds, args, true)
	return source{cols: rollupCols, from: "daily", where: and(conds), args: args, timeEx: "day"}
}

// pickHourly answers from the hourly rollup (hour granularity, no account/key).
func (f Filter) pickHourly() source {
	var conds []string
	var args []any
	if !f.Since.IsZero() {
		conds = append(conds, "hour >= ?")
		args = append(args, f.Since.Unix()/3600)
	}
	if !f.Until.IsZero() {
		conds = append(conds, "hour < ?")
		args = append(args, (f.Until.Unix()+3599)/3600)
	}
	conds, args = f.dimConds(conds, args, false)
	return source{cols: rollupCols, from: "hourly", where: and(conds), args: args,
		timeEx: "strftime('%Y-%m-%d %H', hour*3600, 'unixepoch', 'localtime')"}
}

// pickRequests answers exactly from the request rows (any range, any filter).
func (f Filter) pickRequests() source {
	var conds []string
	var args []any
	if !f.Since.IsZero() {
		conds = append(conds, "at >= ?")
		args = append(args, epoch(f.Since))
	}
	if !f.Until.IsZero() {
		conds = append(conds, "at < ?")
		args = append(args, epoch(f.Until))
	}
	conds, args = f.dimConds(conds, args, true)
	return source{cols: requestCols, from: "requests", where: and(conds), args: args,
		timeEx: "strftime('%Y-%m-%d', at, 'unixepoch', 'localtime')"}
}

// pick chooses the cheapest exact source for a plain or grouped query.
func (f Filter) pick() source {
	if f.aligned() {
		return f.pickDaily()
	}
	return f.pickRequests()
}

// localInterval renders the current fixed UTC offset as a PostgreSQL interval.
// The panel's day/hour buckets are local wall time, matching the day strings
// written by the rollups. A fixed offset is exact except across a DST change;
// the app's deployment timezone (Asia/Shanghai in compose) has no DST.
func localInterval() string {
	_, off := time.Now().Zone()
	sign := "+"
	if off < 0 {
		sign = "-"
		off = -off
	}
	return fmt.Sprintf("interval '%s%02d:%02d:%02d'", sign, off/3600, (off%3600)/60, off%60)
}

func (d *DB) requestDayExpr() string {
	if d.driver != "pg" {
		return "strftime('%Y-%m-%d', at, 'unixepoch', 'localtime')"
	}
	return "to_char((to_timestamp(at) AT TIME ZONE 'UTC') + " + localInterval() + ", 'YYYY-MM-DD')"
}

func (d *DB) requestHourExpr() string {
	if d.driver != "pg" {
		return "strftime('%Y-%m-%d %H', at, 'unixepoch', 'localtime')"
	}
	return "to_char((to_timestamp(at) AT TIME ZONE 'UTC') + " + localInterval() + ", 'YYYY-MM-DD HH24')"
}

func (d *DB) hourlyBucketExpr() string {
	if d.driver != "pg" {
		return "strftime('%Y-%m-%d %H', hour*3600, 'unixepoch', 'localtime')"
	}
	return "to_char((to_timestamp(hour * 3600) AT TIME ZONE 'UTC') + " + localInterval() + ", 'YYYY-MM-DD HH24')"
}

type scanner interface{ Scan(dest ...any) error }

func scanAgg(s scanner, extra ...any) (Agg, error) {
	var a Agg
	var req, ok, ab, fl, pr, co, re, ca, to sql.NullInt64
	var credit sql.NullFloat64
	var elSum, ttSum, ttN, gMs, gTok sql.NullInt64
	dest := append(append([]any{}, extra...), &req, &ok, &ab, &fl, &pr, &co, &re, &ca, &to, &credit, &elSum, &ttSum, &ttN, &gMs, &gTok)
	if err := s.Scan(dest...); err != nil {
		return a, err
	}
	a.Requests, a.OK, a.Aborted, a.Failed = req.Int64, ok.Int64, ab.Int64, fl.Int64
	a.Prompt, a.Completion, a.Reasoning, a.Cached, a.Total = pr.Int64, co.Int64, re.Int64, ca.Int64, to.Int64
	a.Credit = credit.Float64
	if a.Requests > 0 {
		a.AvgElapsedMs = float64(elSum.Int64) / float64(a.Requests)
	}
	if ttN.Int64 > 0 {
		a.AvgTTFTMs = float64(ttSum.Int64) / float64(ttN.Int64)
	}
	if gMs.Int64 > 0 {
		a.TPS = float64(gTok.Int64) / (float64(gMs.Int64) / 1000)
	}
	if a.Prompt > 0 {
		a.CacheHitPct = float64(a.Cached) * 100 / float64(a.Prompt)
	}
	return a, nil
}

// Totals aggregates everything matching f.
func (d *DB) Totals(f Filter) (Agg, error) {
	s := f.pick()
	return scanAgg(d.queryRow("SELECT "+s.cols+" FROM "+s.from+s.where, s.args...))
}

// GroupRow is one row of Group.
type GroupRow struct {
	Key string
	Agg
}

var groupCols = map[string]string{"model": "model", "account": "account", "key": "key_id", "realm": "realm"}

// Group aggregates by model, account, key or realm, biggest token consumers first.
func (d *DB) Group(by string, f Filter) ([]GroupRow, error) {
	col, ok := groupCols[by]
	if !ok {
		return nil, fmt.Errorf("usagedb: cannot group by %q", by)
	}
	s := f.pick()
	totalExpr := "SUM(total)"
	rows, err := d.query("SELECT "+col+", "+s.cols+" FROM "+s.from+s.where+" GROUP BY "+col+" ORDER BY "+totalExpr+" DESC, "+col, s.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupRow
	for rows.Next() {
		var key string
		a, err := scanAgg(rows, &key)
		if err != nil {
			return nil, err
		}
		out = append(out, GroupRow{Key: key, Agg: a})
	}
	return out, rows.Err()
}

// PairPoint is one request's contribution to its (account, model) pair.
type PairPoint struct {
	Account string
	Model   string
	At      float64
	Total   int64
}

// PairPoints returns the per-request token rows in [since, until), oldest
// first. The remaining-usage estimate folds these into a 24h window.
func (d *DB) PairPoints(since, until time.Time) ([]PairPoint, error) {
	rows, err := d.query(`SELECT account, model, at, total FROM requests
		WHERE at >= ? AND at < ? AND account <> '' AND model <> ''
		  AND outcome NOT IN ('client_aborted', 'error')
		ORDER BY at`,
		epoch(since), epoch(until))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PairPoint
	for rows.Next() {
		var p PairPoint
		var total sql.NullInt64
		if err := rows.Scan(&p.Account, &p.Model, &p.At, &total); err != nil {
			return nil, err
		}
		p.Total = total.Int64
		out = append(out, p)
	}
	return out, rows.Err()
}

// SeriesPoint is one time bucket.
type SeriesPoint struct {
	Bucket string // "2026-10-19 14" (hour) or "2026-10-19" (day), local time
	Agg
}

// Series buckets by local hour or day, oldest first.
func (d *DB) Series(bucket string, f Filter) ([]SeriesPoint, error) {
	var s source
	switch bucket {
	case "day":
		s = f.pick()
		if s.from == "requests" {
			s.timeEx = d.requestDayExpr()
		}
	case "hour":
		if f.Account == "" && f.KeyID == "" {
			s = f.pickHourly()
			s.timeEx = d.hourlyBucketExpr()
		} else {
			s = f.pickRequests()
			s.timeEx = d.requestHourExpr()
		}
	default:
		return nil, fmt.Errorf("usagedb: bucket must be hour or day, got %q", bucket)
	}
	rows, err := d.query("SELECT "+s.timeEx+" AS b, "+s.cols+" FROM "+s.from+s.where+" GROUP BY b ORDER BY b", s.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeriesPoint
	for rows.Next() {
		var b string
		a, err := scanAgg(rows, &b)
		if err != nil {
			return nil, err
		}
		out = append(out, SeriesPoint{Bucket: b, Agg: a})
	}
	return out, rows.Err()
}

// Request is one stored request, newest first in Recent.
type Request struct {
	ID int64
	Event
	TPS float64 // completion tokens per second of generation, 0 when unknown
}

// Recent returns up to limit requests with id < beforeID (0 = from the newest).
func (d *DB) Recent(limit int, beforeID int64, f Filter) ([]Request, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	s := f.pickRequests()
	conds := []string{}
	args := append([]any{}, s.args...)
	if s.where != "" {
		conds = append(conds, strings.TrimPrefix(s.where, " WHERE "))
	}
	if beforeID > 0 {
		conds = append(conds, "id < ?")
		args = append(args, beforeID)
	}
	q := `SELECT id, at, model, account, realm, key_id, stream, outcome, status, elapsed_ms, ttft_ms, gen_ms, held_ms,
	  prompt, completion, reasoning, cached, total, credit, effort, estimated, error, extra FROM requests` + and(conds) + " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := d.query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		var r Request
		var at float64
		var stream, est int
		var ttft, gen sql.NullInt64
		var extra sql.NullString
		if err := rows.Scan(&r.ID, &at, &r.Model, &r.Account, &r.Realm, &r.KeyID, &stream, &r.Outcome, &r.Status, &r.ElapsedMs,
			&ttft, &gen, &r.HeldMs, &r.Prompt, &r.Completion, &r.Reasoning, &r.Cached, &r.Total, &r.Credit, &r.Effort,
			&est, &r.Error, &extra); err != nil {
			return nil, err
		}
		r.At = time.Unix(0, int64(at*1e9))
		r.Stream, r.Estimated = stream == 1, est == 1
		if ttft.Valid {
			v := ttft.Int64
			r.TTFTMs = &v
		}
		if gen.Valid {
			v := gen.Int64
			r.GenMs = &v
			if v > 0 && !r.Estimated {
				r.TPS = float64(r.Completion) / (float64(v) / 1000)
			}
		}
		if extra.Valid {
			r.Extra = []byte(extra.String)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DayCounters are one day's consumption, the input of the daily limit guards.
type DayCounters struct {
	RequestsByAccount    map[string]int64
	TokensByAccount      map[string]int64
	CreditsByAccount     map[string]float64
	TokensByAccountModel map[string]map[string]int64
}

// Day computes the counters for requests at or after start (normally local
// midnight) from the daily rollup when start is a day boundary, otherwise from
// the exact request rows.
func (d *DB) Day(start time.Time) (DayCounters, error) {
	out := DayCounters{
		RequestsByAccount: map[string]int64{}, TokensByAccount: map[string]int64{}, CreditsByAccount: map[string]float64{},
		TokensByAccountModel: map[string]map[string]int64{},
	}
	var rows *sql.Rows
	var err error
	if isMidnight(start) {
		rows, err = d.query(`SELECT account, model, SUM(total), SUM(credit), SUM(requests) FROM daily WHERE day >= ? AND account <> ''
		  GROUP BY account, model`, localDay(start))
	} else {
		rows, err = d.query(`SELECT account, model, SUM(total), SUM(credit), COUNT(*) FROM requests WHERE at >= ? AND account <> ''
		  GROUP BY account, model`, epoch(start))
	}
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var acc, model string
		var tok, reqs int64
		var cr float64
		if err := rows.Scan(&acc, &model, &tok, &cr, &reqs); err != nil {
			return out, err
		}
		out.RequestsByAccount[acc] += reqs
		out.TokensByAccount[acc] += tok
		out.CreditsByAccount[acc] += cr
		if out.TokensByAccountModel[acc] == nil {
			out.TokensByAccountModel[acc] = map[string]int64{}
		}
		out.TokensByAccountModel[acc][model] += tok
	}
	return out, rows.Err()
}

// LocalMidnight returns 00:00 of t's local day.
func LocalMidnight(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
