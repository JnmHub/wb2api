// Package api holds the dashboard's JSON endpoints (/api/v2/...). Every handler
// here sits behind panelauth.Auth.Require.
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wb2api/internal/gateway/openai"
	"wb2api/internal/pool"
	"wb2api/internal/pricing"
	"wb2api/internal/usage"
	"wb2api/internal/usagedb"
	"wb2api/internal/usageest"
)

// EventFromGateway converts a finished gateway request into a usage row.
// realmOf maps an account uid to its realm (nil = unknown).
func EventFromGateway(ev openai.UsageEvent, realmOf func(uid string) string) usagedb.Event {
	e := usagedb.Event{
		At: ev.At, Model: ev.Model, Account: ev.Account, KeyID: ev.KeyID, Stream: ev.Stream, Outcome: ev.Outcome,
		Status: ev.Status, ElapsedMs: ev.Elapsed.Milliseconds(), HeldMs: ev.HeldFor.Milliseconds(), Estimated: ev.Estimated, Effort: ev.Effort,
	}
	if realmOf != nil && ev.Account != "" {
		e.Realm = realmOf(ev.Account)
	}
	if ev.TTFT > 0 {
		v := ev.TTFT.Milliseconds()
		e.TTFTMs = &v
	}
	if ev.Gen > 0 {
		v := ev.Gen.Milliseconds()
		e.GenMs = &v
	}
	if t, ok := usage.Extract(ev.Usage); ok {
		e.Prompt, e.Completion, e.Reasoning, e.Cached, e.Total, e.Credit = t.Prompt, t.Completion, t.Reasoning, t.Cached, t.Total, t.Credit
	}
	if ev.Outcome == "failed" && ev.Status >= 400 {
		e.Error = http.StatusText(ev.Status)
	}
	return e
}

// RealmOf returns a lookup suitable for EventFromGateway.
func RealmOf(p *pool.Pool) func(string) string {
	return func(uid string) string {
		if a := p.Get(uid); a != nil {
			return a.Realm
		}
		return ""
	}
}

// Usage serves the usage endpoints.
type Usage struct {
	DB  *usagedb.DB
	Now func() time.Time
	// Est is the remaining-usage estimate (nil = not available).
	Est *usageest.Estimator
	// Pricing estimates API-equivalent cost from OpenRouter's catalogue.
	Pricing *pricing.Store
}

func (u *Usage) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": msg, "type": "invalid_request_error"}})
}

// FilterFromQuery turns ?range=today|week|month|all (or since/until as
// YYYY-MM-DD or RFC3339) plus ?realm= ?model= ?account= ?key= into a Filter.
// Week starts on Monday, month on the 1st, both at local midnight.
func (u *Usage) FilterFromQuery(r *http.Request) (usagedb.Filter, error) {
	q := r.URL.Query()
	now := u.now()
	mid := usagedb.LocalMidnight(now)
	f := usagedb.Filter{Realm: q.Get("realm"), Model: q.Get("model"), Account: q.Get("account"), KeyID: q.Get("key")}
	if f.Realm == "all" {
		f.Realm = ""
	}
	switch q.Get("range") {
	case "", "today":
		f.Since = mid
	case "week":
		back := (int(mid.Weekday()) + 6) % 7
		f.Since = mid.AddDate(0, 0, -back)
	case "month":
		f.Since = time.Date(mid.Year(), mid.Month(), 1, 0, 0, 0, 0, mid.Location())
	case "all":
	case "custom":
	default:
		return f, errBadRange
	}
	parse := func(s string, endOfDay bool) (time.Time, error) {
		if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
			if endOfDay {
				t = t.AddDate(0, 0, 1)
			}
			return t, nil
		}
		return time.Parse(time.RFC3339, s)
	}
	if s := q.Get("since"); s != "" {
		t, err := parse(s, false)
		if err != nil {
			return f, errBadRange
		}
		f.Since = t
	}
	if s := q.Get("until"); s != "" {
		t, err := parse(s, true)
		if err != nil {
			return f, errBadRange
		}
		f.Until = t
	}
	return f, nil
}

type rangeError string

func (e rangeError) Error() string { return string(e) }

const errBadRange = rangeError("range must be today, week, month, all or custom; since/until must be YYYY-MM-DD or RFC3339")

func aggJSON(a usagedb.Agg) map[string]any {
	return map[string]any{
		"requests": a.Requests, "ok": a.OK, "aborted": a.Aborted, "failed": a.Failed,
		"prompt_tokens": a.Prompt, "completion_tokens": a.Completion, "reasoning_tokens": a.Reasoning,
		"cached_tokens": a.Cached, "total_tokens": a.Total, "credit": a.Credit,
		"avg_elapsed_ms": a.AvgElapsedMs, "avg_ttft_ms": a.AvgTTFTMs, "tokens_per_sec": a.TPS, "cache_hit_pct": a.CacheHitPct,
	}
}

// Register mounts the endpoints; wrap is panelauth.Auth.Require.
func (u *Usage) Register(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mux.Handle("GET /api/v2/usage/summary", wrap(http.HandlerFunc(u.summary)))
	mux.Handle("GET /api/v2/usage/series", wrap(http.HandlerFunc(u.series)))
	mux.Handle("GET /api/v2/usage/recent", wrap(http.HandlerFunc(u.recent)))
	mux.Handle("GET /api/v2/usage/remaining", wrap(http.HandlerFunc(u.remaining)))
	mux.Handle("GET /api/v2/pricing", wrap(http.HandlerFunc(u.pricingGet)))
	mux.Handle("POST /api/v2/pricing/refresh", wrap(http.HandlerFunc(u.pricingRefresh)))
	mux.Handle("POST /api/v2/pricing/policy", wrap(http.HandlerFunc(u.pricingPolicy)))
	mux.Handle("POST /api/v2/pricing/mapping", wrap(http.HandlerFunc(u.pricingPolicy)))
	mux.Handle("GET /api/v2/pricing/cost", wrap(http.HandlerFunc(u.pricingCost)))
}

// remaining serves the 24h quota estimate; the ETag makes the panel's poll a
// 304 while nothing changed.
func (u *Usage) remaining(w http.ResponseWriter, r *http.Request) {
	if u.Est == nil {
		writeErr(w, http.StatusServiceUnavailable, "剩余用量估算不可用")
		return
	}
	p := u.Est.Payload()
	tag := `"` + p.ETag + `"`
	inm := strings.TrimSpace(r.Header.Get("If-None-Match"))
	if inm == "*" || inm == tag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", tag)
	writeJSON(w, http.StatusOK, p)
}

func (u *Usage) summary(w http.ResponseWriter, r *http.Request) {
	f, err := u.FilterFromQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	top := 50
	if n, err := strconv.Atoi(r.URL.Query().Get("top")); err == nil && n > 0 && n <= 1000 {
		top = n
	}
	total, err := u.DB.Totals(f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := map[string]any{"totals": aggJSON(total)}
	for _, by := range []string{"model", "account", "key", "realm"} {
		rows, err := u.DB.Group(by, f)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		truncated := len(rows) > top
		if truncated {
			rows = rows[:top]
		}
		list := make([]map[string]any, 0, len(rows))
		for _, g := range rows {
			m := aggJSON(g.Agg)
			m["key"] = g.Key
			list = append(list, m)
		}
		out["by_"+by] = list
		out["by_"+by+"_truncated"] = truncated
	}
	qs := u.DB.Stats()
	out["queue"] = map[string]any{
		"written": qs.Written, "dropped": qs.Dropped, "last_error": qs.LastError,
		"depth": qs.QueueDepth, "capacity": qs.QueueCapacity, "workers": qs.Workers,
		"batches": qs.Batches, "failed": qs.Failed, "overflow": qs.Overflow,
	}
	writeJSON(w, http.StatusOK, out)
}

func (u *Usage) series(w http.ResponseWriter, r *http.Request) {
	f, err := u.FilterFromQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" {
		bucket = "hour"
	}
	pts, err := u.DB.Series(bucket, f)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	list := make([]map[string]any, 0, len(pts))
	for _, p := range pts {
		m := aggJSON(p.Agg)
		m["bucket"] = p.Bucket
		list = append(list, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"bucket": bucket, "points": list})
}

func (u *Usage) recent(w http.ResponseWriter, r *http.Request) {
	f, err := u.FilterFromQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	f.Since = timeZeroIfDefault(r, f.Since)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	rows, err := u.DB.Recent(limit, before, f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	list := make([]map[string]any, 0, len(rows))
	var next int64
	for _, q := range rows {
		m := map[string]any{
			"id": q.ID, "at": q.At.UnixMilli(), "model": q.Model, "account": q.Account, "realm": q.Realm, "key": q.KeyID,
			"stream": q.Stream, "outcome": q.Outcome, "status": q.Status, "elapsed_ms": q.ElapsedMs,
			"prompt_tokens": q.Prompt, "completion_tokens": q.Completion, "reasoning_tokens": q.Reasoning,
			"cached_tokens": q.Cached, "total_tokens": q.Total, "credit": q.Credit, "reasoning_effort": q.Effort,
			"usage_estimated": q.Estimated, "error": q.Error, "tokens_per_sec": q.TPS, "held_ms": q.HeldMs,
		}
		if q.TTFTMs != nil {
			m["ttft_ms"] = *q.TTFTMs
		}
		if q.GenMs != nil {
			m["gen_ms"] = *q.GenMs
		}
		list = append(list, m)
		next = q.ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": list, "next_before": next})
}

// timeZeroIfDefault: the request list is not limited to today unless asked.
func timeZeroIfDefault(r *http.Request, t time.Time) time.Time {
	if r.URL.Query().Get("range") == "" && r.URL.Query().Get("since") == "" {
		return time.Time{}
	}
	return t
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func jsonIndent(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
