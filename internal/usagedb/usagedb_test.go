package usagedb

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func open(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "usage.db"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func i64(v int64) *int64 { return &v }

func TestWriterStatsReflectOptions(t *testing.T) {
	d, err := OpenWithOptions(filepath.Join(t.TempDir(), "usage.db"),
		Options{QueueSize: 32, Workers: 3, Overflow: OverflowSample, SampleEvery: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	st := d.Stats()
	if st.QueueCapacity != 32 || st.Workers != 3 || st.Overflow != "sample" {
		t.Fatalf("unexpected stats: %+v", st)
	}
	d.Record(Event{At: time.Now(), Model: "m", Outcome: "completed", Total: 7})
	d.Flush()
	st = d.Stats()
	if st.Written != 1 || st.Dropped != 0 || st.Batches == 0 {
		t.Fatalf("after flush: %+v", st)
	}
}

func TestSampleOverflowKeepsAccountingComplete(t *testing.T) {
	d, err := OpenWithOptions(filepath.Join(t.TempDir(), "usage.db"),
		Options{QueueSize: 8, Workers: 1, Overflow: OverflowSample, SampleEvery: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// Hold SQLite's write lock so the queue backs up and sampling kicks in.
	tx, err := d.sql.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO meta(k, v) VALUES('lock','1')"); err != nil {
		t.Fatal(err)
	}
	const total = 2000
	for i := 0; i < total; i++ {
		d.Record(Event{At: time.Now(), Model: "m", Outcome: "completed", Total: 1})
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	d.Flush()
	st := d.Stats()
	if st.Dropped == 0 {
		t.Fatalf("sample mode should have dropped some events under pressure: %+v", st)
	}
	if st.Written+st.Dropped != total {
		t.Fatalf("written %d + dropped %d != %d", st.Written, st.Dropped, total)
	}
}

type expAgg struct {
	Requests   int64   `json:"requests"`
	OK         int64   `json:"ok"`
	Aborted    int64   `json:"aborted"`
	Failed     int64   `json:"failed"`
	Prompt     int64   `json:"prompt"`
	Completion int64   `json:"completion"`
	Reasoning  int64   `json:"reasoning"`
	Cached     int64   `json:"cached"`
	Total      int64   `json:"total"`
	Credit     float64 `json:"credit"`
	GenMs      int64   `json:"gen_ms"`
	GenTokens  int64   `json:"gen_tokens"`
}

func (e expAgg) check(t *testing.T, label string, a Agg) {
	t.Helper()
	got := expAgg{a.Requests, a.OK, a.Aborted, a.Failed, a.Prompt, a.Completion, a.Reasoning, a.Cached, a.Total, a.Credit, 0, 0}
	want := e
	want.GenMs, want.GenTokens = 0, 0
	if math.Abs(got.Credit-want.Credit) > 1e-6 {
		t.Errorf("%s credit: got %v want %v", label, got.Credit, want.Credit)
	}
	got.Credit, want.Credit = 0, 0
	if got != want {
		t.Errorf("%s:\n got  %+v\n want %+v", label, got, want)
	}
	if e.GenMs > 0 {
		wantTPS := float64(e.GenTokens) / (float64(e.GenMs) / 1000)
		if math.Abs(a.TPS-wantTPS) > 1e-6 {
			t.Errorf("%s TPS: got %v want %v", label, a.TPS, wantTPS)
		}
	}
}

// The legacy sample log, imported into SQLite, must aggregate to exactly what
// plain Python arithmetic over the same file gives - overall, per model, per account.
func TestImportMatchesIndependentAggregates(t *testing.T) {
	raw, err := os.ReadFile("testdata/sample_expected.json")
	if err != nil {
		t.Fatal(err)
	}
	var exp struct {
		Rows      int               `json:"rows"`
		Total     expAgg            `json:"total"`
		ByModel   map[string]expAgg `json:"by_model"`
		ByAccount map[string]expAgg `json:"by_account"`
	}
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatal(err)
	}
	d := open(t)
	n, err := d.ImportJSONL(context.Background(), "../../legacy/python/tests/fixtures/pricing_tooltip/usage_sample.jsonl", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != exp.Rows {
		t.Fatalf("imported %d rows, want %d", n, exp.Rows)
	}
	tot, err := d.Totals(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	exp.Total.check(t, "total", tot)

	byModel, _ := d.Group("model", Filter{})
	if len(byModel) != len(exp.ByModel) {
		t.Fatalf("models: got %d want %d", len(byModel), len(exp.ByModel))
	}
	for _, r := range byModel {
		e, ok := exp.ByModel[r.Key]
		if !ok {
			t.Fatalf("unexpected model %q", r.Key)
		}
		e.check(t, "model "+r.Key, r.Agg)
	}
	byAcc, _ := d.Group("account", Filter{})
	if len(byAcc) != len(exp.ByAccount) {
		t.Fatalf("accounts: got %d want %d", len(byAcc), len(exp.ByAccount))
	}
	for _, r := range byAcc {
		e, ok := exp.ByAccount[r.Key]
		if !ok {
			t.Fatalf("unexpected account %q", r.Key)
		}
		e.check(t, "account "+r.Key, r.Agg)
	}
	// the sorted order puts the biggest consumer first
	for i := 1; i < len(byModel); i++ {
		if byModel[i-1].Total < byModel[i].Total {
			t.Error("Group must sort by total tokens, descending")
		}
	}
	// requests table agrees with the rollup and keeps unmapped legacy fields
	rec, err := d.Recent(5, 0, Filter{})
	if err != nil || len(rec) != 5 {
		t.Fatalf("recent: %v %d", err, len(rec))
	}
	if len(rec[0].Extra) == 0 {
		t.Error("legacy fields without a column (msgs_sha, ...) must survive in extra")
	}
}

func TestImportIsResumableAndIdempotent(t *testing.T) {
	d := open(t)
	src := filepath.Join(t.TempDir(), "usage.jsonl")
	write := func(rows ...string) {
		f, err := os.OpenFile(src, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		for _, r := range rows {
			f.WriteString(r)
		}
	}
	row := func(i int) string {
		return fmt.Sprintf(`{"at":%d.5,"model":"m","account":"a","total_tokens":%d,"completion_tokens":1,"outcome":"completed"}`+"\n", 1_790_000_000+i, 10)
	}
	write(row(1), row(2), row(3))
	if n, err := d.ImportJSONL(context.Background(), src, nil); err != nil || n != 3 {
		t.Fatalf("first import: %d %v", n, err)
	}
	if n, _ := d.ImportJSONL(context.Background(), src, nil); n != 0 {
		t.Fatalf("a second run must not import anything again, got %d", n)
	}
	write(row(4), `{"at":1790000005.5,"model":"m","total_tokens":10`) // second one is a half-written line
	if n, _ := d.ImportJSONL(context.Background(), src, nil); n != 1 {
		t.Fatalf("only the new complete row should be imported, got %d", n)
	}
	write("\n") // the writer finishes... but the line is invalid JSON: skipped
	if n, _ := d.ImportJSONL(context.Background(), src, nil); n != 0 {
		t.Fatalf("invalid line imported? %d", n)
	}
	tot, _ := d.Totals(Filter{})
	if tot.Requests != 4 || tot.Total != 40 {
		t.Fatalf("totals %+v", tot)
	}
}

func TestMissingFileImportsNothing(t *testing.T) {
	d := open(t)
	if n, err := d.ImportJSONL(context.Background(), filepath.Join(t.TempDir(), "nope.jsonl"), nil); n != 0 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
}

func TestRecordAggregatesAndRates(t *testing.T) {
	d := open(t)
	base := time.Date(2026, 10, 19, 10, 15, 0, 0, time.Local)
	d.Record(Event{At: base, Model: "m1", Account: "a1", Realm: "intl", KeyID: "k1", Stream: true, Outcome: "completed",
		ElapsedMs: 2000, TTFTMs: i64(400), GenMs: i64(1000), Prompt: 100, Completion: 50, Cached: 40, Total: 150, Credit: 0.5})
	d.Record(Event{At: base.Add(time.Minute), Model: "m1", Account: "a1", Realm: "intl", KeyID: "k1", Outcome: "completed",
		ElapsedMs: 3000, TTFTMs: i64(600), GenMs: i64(1000), Prompt: 100, Completion: 150, Total: 250, Credit: 1})
	d.Record(Event{At: base.Add(2 * time.Minute), Model: "m2", Account: "a2", Realm: "cn", Outcome: "failed", Status: 502, Error: "boom", ElapsedMs: 100})
	d.Record(Event{At: base.Add(3 * time.Minute), Model: "m1", Account: "a1", Realm: "intl", Outcome: "client_aborted", ElapsedMs: 50})
	// an estimated-usage request must not distort the generation rate
	d.Record(Event{At: base.Add(4 * time.Minute), Model: "m1", Account: "a1", Realm: "intl", Outcome: "completed", ElapsedMs: 10,
		GenMs: i64(10), Completion: 999999, Total: 999999, Estimated: true})
	d.Flush()
	if d.Written() != 5 || d.Dropped() != 0 {
		t.Fatalf("written=%d dropped=%d", d.Written(), d.Dropped())
	}
	all, _ := d.Totals(Filter{})
	if all.Requests != 5 || all.OK != 3 || all.Aborted != 1 || all.Failed != 1 {
		t.Fatalf("outcome counts: %+v", all)
	}
	m1, _ := d.Totals(Filter{Model: "m1", Realm: "intl"})
	// (50+150) tokens over (1000+1000) ms = 100 tok/s; the estimated row is excluded
	if math.Abs(m1.TPS-100) > 1e-9 {
		t.Errorf("TPS = %v, want 100 (token-weighted, estimates excluded)", m1.TPS)
	}
	if math.Abs(m1.AvgTTFTMs-500) > 1e-9 {
		t.Errorf("avg TTFT = %v, want 500", m1.AvgTTFTMs)
	}
	if want := 40.0 * 100 / (100 + 100); math.Abs(m1.CacheHitPct-want) > 1e-9 {
		t.Errorf("cache hit = %v want %v", m1.CacheHitPct, want)
	}
	if _, err := d.Group("nonsense", Filter{}); err == nil {
		t.Error("grouping by an unknown column must be refused (SQL injection guard)")
	}
	rec, _ := d.Recent(2, 0, Filter{Model: "m1"})
	if len(rec) != 2 || rec[0].At.Before(rec[1].At) {
		t.Fatalf("recent must be newest first: %+v", rec)
	}
	if rec[0].Estimated != true {
		t.Errorf("newest m1 row is the estimated one")
	}
	older, _ := d.Recent(10, rec[1].ID, Filter{})
	for _, r := range older {
		if r.ID >= rec[1].ID {
			t.Fatal("beforeID cursor not honoured")
		}
	}
	// the failed request keeps its error text
	fr, _ := d.Recent(10, 0, Filter{Model: "m2"})
	if len(fr) != 1 || fr[0].Error != "boom" || fr[0].Status != 502 {
		t.Fatalf("%+v", fr)
	}
}

func TestSeriesBuckets(t *testing.T) {
	d := open(t)
	day := time.Date(2026, 10, 19, 23, 30, 0, 0, time.Local)
	d.Record(Event{At: day, Model: "m", Outcome: "completed", Total: 1})
	d.Record(Event{At: day.Add(time.Hour), Model: "m", Outcome: "completed", Total: 2}) // next local day
	d.Record(Event{At: day.Add(time.Hour + time.Minute), Model: "m", Outcome: "completed", Total: 4})
	d.Flush()
	days, err := d.Series("day", Filter{})
	if err != nil || len(days) != 2 || days[0].Bucket != "2026-10-19" || days[1].Bucket != "2026-10-20" || days[1].Total != 6 {
		t.Fatalf("%+v %v", days, err)
	}
	hours, _ := d.Series("hour", Filter{})
	if len(hours) != 2 || hours[0].Bucket != "2026-10-19 23" || hours[1].Bucket != "2026-10-20 00" {
		t.Fatalf("%+v", hours)
	}
	if _, err := d.Series("week", Filter{}); err == nil {
		t.Error("unknown bucket must error")
	}
	only, _ := d.Totals(Filter{Since: time.Date(2026, 10, 20, 0, 0, 0, 0, time.Local)})
	if only.Total != 6 {
		t.Errorf("since filter: %+v", only)
	}
}

func TestDayCounters(t *testing.T) {
	d := open(t)
	now := time.Now()
	start := LocalMidnight(now)
	d.Record(Event{At: start.Add(-time.Hour), Account: "a", Model: "m", Total: 1000, Credit: 9}) // yesterday
	d.Record(Event{At: start.Add(time.Minute), Account: "a", Model: "m", Total: 10, Credit: 1})
	d.Record(Event{At: start.Add(2 * time.Minute), Account: "a", Model: "n", Total: 20, Credit: 2})
	d.Record(Event{At: start.Add(3 * time.Minute), Account: "b", Model: "m", Total: 5, Credit: 0.5})
	d.Record(Event{At: start.Add(4 * time.Minute), Account: "", Model: "m", Total: 77}) // unattributed
	d.Flush()
	dc, err := d.Day(start)
	if err != nil {
		t.Fatal(err)
	}
	if dc.TokensByAccount["a"] != 30 || dc.TokensByAccount["b"] != 5 || len(dc.TokensByAccount) != 2 {
		t.Fatalf("%+v", dc.TokensByAccount)
	}
	if dc.CreditsByAccount["a"] != 3 || dc.TokensByAccountModel["a"]["n"] != 20 || dc.TokensByAccountModel["a"]["m"] != 10 {
		t.Fatalf("%+v %+v", dc.CreditsByAccount, dc.TokensByAccountModel)
	}
}

func TestRecordNeverBlocksAndCountsDrops(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "u.db"), 1) // tiny queue
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// Hold the writer busy so the queue fills: take the only write transaction.
	tx, _ := d.sql.Begin()
	_, _ = tx.Exec("INSERT INTO meta(k, v) VALUES('lock','1')")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5000; i++ {
			d.Record(Event{At: time.Now(), Model: "m", Outcome: "completed"})
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Record blocked the caller")
	}
	tx.Rollback()
	d.Flush()
	if d.Dropped() == 0 {
		t.Error("with a 1-slot queue and a stalled writer some events must be dropped (and counted)")
	}
	if d.Written()+d.Dropped() != 5000 {
		t.Errorf("written %d + dropped %d != 5000", d.Written(), d.Dropped())
	}
}

func TestConcurrentWritersAndReaders(t *testing.T) {
	d := open(t)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				d.Record(Event{At: time.Now(), Model: fmt.Sprintf("m%d", g%3), Account: fmt.Sprintf("a%d", g), Outcome: "completed", Total: 1})
				if i%100 == 0 {
					_, _ = d.Totals(Filter{})
				}
			}
		}(g)
	}
	wg.Wait()
	d.Flush()
	tot, _ := d.Totals(Filter{})
	if tot.Requests+d.Dropped() != 4000 || tot.Requests != d.Written() {
		t.Fatalf("requests=%d written=%d dropped=%d", tot.Requests, d.Written(), d.Dropped())
	}
	var rows int
	d.SQL().QueryRow("SELECT COUNT(*) FROM requests").Scan(&rows)
	if int64(rows) != tot.Requests {
		t.Fatalf("requests table (%d) and rollup (%d) disagree", rows, tot.Requests)
	}
}

// The two ways of answering a query - the pre-aggregated rollups and the exact
// scan of request rows - must agree.
func TestRollupAndExactPathsAgree(t *testing.T) {
	d := open(t)
	if _, err := d.ImportJSONL(context.Background(), "../../legacy/python/tests/fixtures/pricing_tooltip/usage_sample.jsonl", nil); err != nil {
		t.Fatal(err)
	}
	rolled, err := d.Totals(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	// An unaligned bound forces the exact path over the same rows.
	exact, err := d.Totals(Filter{Since: time.Unix(1, 0).Add(7 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	same := func(label string, a, b Agg) {
		t.Helper()
		if a.Requests != b.Requests || a.OK != b.OK || a.Failed != b.Failed || a.Aborted != b.Aborted || a.Total != b.Total ||
			a.Prompt != b.Prompt || a.Completion != b.Completion || a.Cached != b.Cached ||
			math.Abs(a.Credit-b.Credit) > 1e-6 || math.Abs(a.TPS-b.TPS) > 1e-9 ||
			math.Abs(a.AvgTTFTMs-b.AvgTTFTMs) > 1e-9 || math.Abs(a.AvgElapsedMs-b.AvgElapsedMs) > 1e-9 {
			t.Errorf("%s differ:\n rollup %+v\n exact  %+v", label, a, b)
		}
	}
	same("totals", rolled, exact)
	byModelRolled, _ := d.Group("model", Filter{})
	byModelExact, _ := d.Group("model", Filter{Since: time.Unix(1, 0).Add(7 * time.Second)})
	if len(byModelRolled) != len(byModelExact) {
		t.Fatalf("group sizes %d vs %d", len(byModelRolled), len(byModelExact))
	}
	exactByKey := map[string]Agg{}
	for _, r := range byModelExact {
		exactByKey[r.Key] = r.Agg
	}
	for _, r := range byModelRolled {
		same("model "+r.Key, r.Agg, exactByKey[r.Key])
	}
	// per-account hourly series (exact path) sums to the same total as the rollup series
	hourly, _ := d.Series("hour", Filter{})
	var sum int64
	for _, p := range hourly {
		sum += p.Total
	}
	if sum != rolled.Total {
		t.Errorf("hourly series sums to %d, total is %d", sum, rolled.Total)
	}
	accounts, _ := d.Group("account", Filter{})
	acct := accounts[0].Key
	byAcctHourly, err := d.Series("hour", Filter{Account: acct})
	if err != nil {
		t.Fatal(err)
	}
	var accSum int64
	for _, p := range byAcctHourly {
		accSum += p.Total
	}
	if accSum != accounts[0].Total {
		t.Errorf("account %s: hourly series %d vs group total %d", acct, accSum, accounts[0].Total)
	}
}

func TestPostgresSchemaUsesSharedTables(t *testing.T) {
	s := postgresSchema()
	for _, want := range []string{"id          BIGSERIAL PRIMARY KEY", "DOUBLE PRECISION", "CREATE TABLE IF NOT EXISTS requests"} {
		if !strings.Contains(s, want) {
			t.Errorf("postgres schema missing %q", want)
		}
	}
	if strings.Contains(s, "WITHOUT ROWID") {
		t.Fatal("postgres schema still has SQLite-only WITHOUT ROWID")
	}
}

func TestBindPostgresPlaceholders(t *testing.T) {
	d := &DB{driver: "pg"}
	got := d.bind("SELECT a FROM t WHERE x = ? AND y = ?")
	if got != "SELECT a FROM t WHERE x = $1 AND y = $2" {
		t.Fatalf("bind = %q", got)
	}
	if got := (&DB{driver: "sqlite"}).bind("x = ?"); got != "x = ?" {
		t.Fatalf("sqlite bind changed: %q", got)
	}
}

func TestSQLiteRowToEvent(t *testing.T) {
	at := float64(time.Now().Add(-time.Minute).UnixNano()) / 1e9
	ev, id, ok := sqliteRowToEvent(map[string]any{
		"id": int64(7), "at": at, "model": "legacy", "account": "a1", "key": "k1",
		"stream": int64(1), "outcome": "completed", "status": int64(200),
		"elapsed_ms": int64(1200), "ttft_ms": int64(300), "prompt": int64(10),
		"completion": int64(4), "credit": 0.5, "estimated": int64(0),
	})
	if !ok || id != 7 || ev.Model != "legacy" || ev.KeyID != "k1" || !ev.Stream {
		t.Fatalf("event = %+v, id=%d ok=%v", ev, id, ok)
	}
	if ev.TTFTMs == nil || *ev.TTFTMs != 300 || ev.Credit != 0.5 || ev.Realm == "" {
		t.Fatalf("event fields = %+v", ev)
	}
}

func TestImportSQLiteIntoPostgres(t *testing.T) {
	dsn := os.Getenv("WB_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("WB_TEST_POSTGRES_DSN not set")
	}
	path := filepath.Join(t.TempDir(), "usage.db")
	src, err := Open(path, 64)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Minute).Truncate(time.Second)
	src.Record(Event{At: at, Model: "legacy-a", Account: "a1", Realm: "intl", KeyID: "k1",
		Outcome: "completed", Status: 200, Prompt: 10, Completion: 4, Total: 14})
	src.Record(Event{At: at.Add(time.Second), Model: "legacy-b", Account: "a2", Realm: "cn", KeyID: "k2",
		Outcome: "failed", Status: 500, Error: "boom", Prompt: 2, Total: 2})
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := OpenPostgres(dsn, 32)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.sql.Exec("TRUNCATE requests, daily, hourly, meta"); err != nil {
		t.Fatal(err)
	}
	n, err := db.ImportSQLite(context.Background(), path)
	if err != nil || n != 2 {
		t.Fatalf("first import = %d, %v; want 2", n, err)
	}
	if n, err := db.ImportSQLite(context.Background(), path); err != nil || n != 0 {
		t.Fatalf("second import = %d, %v; want 0", n, err)
	}
	rows, err := db.Recent(10, 0, Filter{})
	if err != nil || len(rows) != 2 {
		t.Fatalf("recent = %d, %v", len(rows), err)
	}
	if rows[0].Model != "legacy-b" || rows[0].Error != "boom" {
		t.Fatalf("recent[0] = %+v", rows[0])
	}
	agg, err := db.Totals(Filter{})
	if err != nil || agg.Requests != 2 || agg.Total != 16 {
		t.Fatalf("totals = %+v, %v", agg, err)
	}
}

func TestPostgresRoundTrip(t *testing.T) {
	dsn := os.Getenv("WB_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("WB_TEST_POSTGRES_DSN not set")
	}
	db, err := OpenPostgres(dsn, 32)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.sql.Exec("TRUNCATE requests, daily, hourly, meta"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	at := time.Now().Add(-time.Minute)
	ttft, gen := int64(120), int64(1000)
	db.Record(Event{At: at, Model: "pg-test", Account: "a1", Realm: "intl", KeyID: "k1", Stream: true,
		Outcome: "completed", Status: 200, ElapsedMs: 1200, TTFTMs: &ttft, GenMs: &gen,
		Prompt: 10, Completion: 5, Total: 15})
	db.Record(Event{At: at.Add(time.Second), Model: "pg-test", Account: "a2", Realm: "intl", KeyID: "k2",
		Outcome: "failed", Status: 500, Prompt: 3, Total: 3})
	db.Flush()
	if got := db.Written(); got != 2 {
		t.Fatalf("written = %d, want 2 (last error %q)", got, db.LastError())
	}
	agg, err := db.Totals(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if agg.Requests != 2 || agg.OK != 1 || agg.Failed != 1 || agg.Total != 18 {
		t.Fatalf("totals = %+v", agg)
	}
	rows, err := db.Recent(10, 0, Filter{})
	if err != nil || len(rows) != 2 {
		t.Fatalf("recent = %d, %v", len(rows), err)
	}
	if rows[0].Model != "pg-test" || rows[0].Account != "a2" {
		t.Fatalf("recent[0] = %+v", rows[0])
	}
	series, err := db.Series("hour", Filter{Since: at.Add(-time.Hour), Until: at.Add(time.Hour)})
	if err != nil || len(series) != 1 || series[0].Requests != 2 {
		t.Fatalf("series = %+v, %v", series, err)
	}
	day, err := db.Day(LocalMidnight(at))
	if err != nil || day.RequestsByAccount["a1"] != 1 || day.TokensByAccount["a2"] != 3 {
		t.Fatalf("day = %+v, %v", day, err)
	}
	groups, err := db.Group("account", Filter{})
	if err != nil || len(groups) != 2 {
		t.Fatalf("groups = %+v, %v", groups, err)
	}
}
