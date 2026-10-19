package usagedb

import (
	"bufio"
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestColdStartScale mirrors the legacy measurement (200,000 request log, cold
// cache): how long until the dashboard's aggregate queries answer after import.
// The legacy Python took 16.9 s (snapshot) + 19.4 s (analytics) + 3.3 s (series)
// + 1.2 s (by account) for the same volume. Run with WB_SCALE=1.
func TestColdStartScale(t *testing.T) {
	if os.Getenv("WB_SCALE") != "1" {
		t.Skip("set WB_SCALE=1 to run the 200k-row scale check")
	}
	const n = 200_000
	rng := rand.New(rand.NewSource(2))
	dir := t.TempDir()
	src := filepath.Join(dir, "usage.jsonl")
	f, _ := os.Create(src)
	w := bufio.NewWriter(f)
	models := []string{"glm-5.3", "deepseek-v4.1-flash", "gpt-5.5", "kimi-k2.6", "hy3"}
	now := float64(time.Now().Unix())
	for i := 0; i < n; i++ {
		at := now - rng.Float64()*86400*14
		pt, ct, gen := 2000+rng.Intn(58000), 20+rng.Intn(2000), 500+rng.Intn(19500)
		fmt.Fprintf(w, `{"account":"acct-%04d","at":%f,"cache_hit_pct":%.1f,"cached_tokens":%d,"completion_tokens":%d,"credit":%.3f,"elapsed_ms":%d,"gen_ms":%d,"iso":"x","model":%q,"msgs_sha":"aaaaaaaaaaaa","n_msgs":20,"outcome":"completed","prefix_sha":"bbbbbbbbbbbb","prompt_tokens":%d,"realm":"intl","reasoning_tokens":0,"stream":true,"system_sha":"cccccccccccc","tokens_per_sec":%.2f,"total_tokens":%d,"ttft_ms":%d}`+"\n",
			rng.Intn(300), at, rng.Float64()*90, pt/2, ct, rng.Float64()*3, gen+1200, gen, models[rng.Intn(len(models))], pt, float64(ct)/(float64(gen)/1000), pt+ct, 300+rng.Intn(2700))
	}
	w.Flush()
	f.Close()
	st, _ := os.Stat(src)

	d, err := Open(filepath.Join(dir, "usage.db"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	t0 := time.Now()
	got, err := d.ImportJSONL(context.Background(), src, nil)
	if err != nil || got != n {
		t.Fatalf("import: %d %v", got, err)
	}
	imp := time.Since(t0)

	timeIt := func(name string, fn func() error) time.Duration {
		s := time.Now()
		if err := fn(); err != nil {
			t.Fatal(err)
		}
		return time.Since(s)
	}
	totals := timeIt("totals", func() error { _, e := d.Totals(Filter{}); return e })
	byModel := timeIt("by model", func() error { _, e := d.Group("model", Filter{}); return e })
	byAcc := timeIt("by account", func() error { _, e := d.Group("account", Filter{}); return e })
	series := timeIt("series", func() error { _, e := d.Series("hour", Filter{}); return e })
	recent := timeIt("recent", func() error { _, e := d.Recent(20, 0, Filter{}); return e })
	day := timeIt("day counters", func() error { _, e := d.Day(LocalMidnight(time.Now())); return e })
	t.Logf("log %.0f MB, %d rows, import %v (%.0f rows/s)", float64(st.Size())/1e6, n, imp.Round(time.Millisecond), float64(n)/imp.Seconds())
	t.Logf("totals %v | by model %v | by account %v | hourly series %v | recent(20) %v | today's counters %v",
		totals.Round(time.Microsecond), byModel.Round(time.Microsecond), byAcc.Round(time.Microsecond),
		series.Round(time.Microsecond), recent.Round(time.Microsecond), day.Round(time.Microsecond))
	_ = strconv.Itoa
}
