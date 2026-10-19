// Command loadtest drives many concurrent streaming chat requests against a
// gateway and reports time-to-first-token, per-stream delivery rate and error
// counts. `loadtest genaccounts` writes fake account files for a test pool.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

func genAccounts(args []string) {
	fs := flag.NewFlagSet("genaccounts", flag.ExitOnError)
	dir := fs.String("dir", "accounts", "output directory")
	n := fs.Int("n", 100, "number of accounts")
	key := fs.String("key", "", "if set, also write settings.json with this API key")
	_ = fs.Parse(args)
	_ = os.MkdirAll(*dir, 0o755)
	exp := time.Now().Add(30 * 24 * time.Hour).Unix()
	for i := 0; i < *n; i++ {
		uid := fmt.Sprintf("load-%05d", i)
		doc := map[string]any{
			"uid": uid, "accessToken": "tok-" + uid, "refreshToken": "ref-" + uid, "realm": "intl",
			"domain": "www.workbuddy.ai", "expiresAt": exp, "enabled": true, "weight": 1, "source": "loadtest",
		}
		b, _ := json.MarshalIndent(doc, "", "  ")
		_ = os.WriteFile(filepath.Join(*dir, uid+".json"), b, 0o644)
	}
	if *key != "" {
		b, _ := json.MarshalIndent(map[string]any{"api_keys": []map[string]any{{"id": "k1", "name": "load", "key": *key}}}, "", "  ")
		_ = os.WriteFile(filepath.Join(*dir, "settings.json"), b, 0o644)
	}
	fmt.Printf("wrote %d accounts to %s\n", *n, *dir)
}

type sample struct {
	status   int
	ttft     time.Duration
	total    time.Duration
	chunks   int
	rate     float64 // chunks/s between first and last chunk
	maxGap   time.Duration
	err      string
	finished bool
}

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	i := int(p * float64(len(d)-1))
	return d[i]
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "genaccounts" {
		genAccounts(os.Args[2:])
		return
	}
	url := flag.String("url", "http://127.0.0.1:8788/v1/chat/completions", "gateway URL")
	conc := flag.Int("c", 100, "concurrent streams")
	reqs := flag.Int("n", 0, "total requests (0 = exactly one per worker)")
	model := flag.String("model", "glm-5.3", "model")
	tokens := flag.Int("tokens", 200, "tokens per stream asked of the mock (X-Mock-Tokens)")
	tps := flag.Int("tps", 50, "tokens/s per stream asked of the mock (X-Mock-TPS)")
	key := flag.String("key", "", "API key")
	statsURL := flag.String("stats", "", "gateway /debug/runtime URL to sample")
	ramp := flag.Duration("ramp", 0, "spread stream starts over this duration")
	flag.Parse()

	total := *reqs
	if total == 0 {
		total = *conc
	}
	tr := &http.Transport{MaxIdleConns: *conc * 2, MaxIdleConnsPerHost: *conc * 2, DisableCompression: true}
	client := &http.Client{Transport: tr}
	body, _ := json.Marshal(map[string]any{"model": *model, "stream": true, "messages": []map[string]string{{"role": "user", "content": "hi"}}})

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		samples []sample
		next    atomic.Int64
		peakG   atomic.Int64
	)
	stopSample := make(chan struct{})
	if *statsURL != "" {
		go func() {
			t := time.NewTicker(500 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-stopSample:
					return
				case <-t.C:
					resp, err := http.Get(*statsURL)
					if err != nil {
						continue
					}
					var m map[string]float64
					_ = json.NewDecoder(resp.Body).Decode(&m)
					resp.Body.Close()
					if g := int64(m["goroutines"]); g > peakG.Load() {
						peakG.Store(g)
					}
				}
			}
		}()
	}

	begin := time.Now()
	for w := 0; w < *conc; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			if *ramp > 0 {
				time.Sleep(time.Duration(float64(*ramp) * float64(w) / float64(*conc)))
			}
			for {
				if int(next.Add(1)) > total {
					return
				}
				s := oneStream(client, *url, *key, body, *tokens, *tps)
				mu.Lock()
				samples = append(samples, s)
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	close(stopSample)
	wall := time.Since(begin)

	var ttft, gaps []time.Duration
	var rates []float64
	errs := map[string]int{}
	ok, chunks := 0, 0
	for _, s := range samples {
		if s.err != "" || !s.finished {
			e := s.err
			if e == "" {
				e = "truncated stream (status " + strconv.Itoa(s.status) + ")"
			}
			errs[e]++
			continue
		}
		ok++
		chunks += s.chunks
		ttft = append(ttft, s.ttft)
		gaps = append(gaps, s.maxGap)
		rates = append(rates, s.rate)
	}
	sort.Slice(ttft, func(i, j int) bool { return ttft[i] < ttft[j] })
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	sort.Float64s(rates)
	fmt.Printf("requests      : %d ok / %d total in %v\n", ok, len(samples), wall.Round(time.Millisecond))
	for e, c := range errs {
		fmt.Printf("  error x%-5d: %s\n", c, e)
	}
	if ok > 0 {
		fmt.Printf("TTFT          : p50 %v  p99 %v  max %v\n", pct(ttft, .5).Round(time.Microsecond), pct(ttft, .99).Round(time.Microsecond), ttft[len(ttft)-1].Round(time.Microsecond))
		fmt.Printf("longest stall : p50 %v  p99 %v  max %v (largest gap between two chunks of one stream)\n", pct(gaps, .5).Round(time.Millisecond), pct(gaps, .99).Round(time.Millisecond), gaps[len(gaps)-1].Round(time.Millisecond))
		fmt.Printf("stream rate   : p1 %.1f  p50 %.1f  chunks/s per stream (asked: %d)\n", rates[int(.01*float64(len(rates)-1))], rates[len(rates)/2], *tps)
		fmt.Printf("throughput    : %.0f chunks/s total\n", float64(chunks)/wall.Seconds())
	}
	if *statsURL != "" {
		fmt.Printf("gateway peak goroutines (sampled): %d\n", peakG.Load())
		if resp, err := http.Get(*statsURL); err == nil {
			var m map[string]float64
			_ = json.NewDecoder(resp.Body).Decode(&m)
			resp.Body.Close()
			fmt.Printf("gateway memory: heap %.0f MB, sys %.0f MB, gc pauses %.1f ms\n", m["heap_alloc"]/1e6, m["sys"]/1e6, m["pause_total_ns"]/1e6)
		}
	}
}

func oneStream(c *http.Client, url, key string, body []byte, tokens, tps int) sample {
	var s sample
	start := time.Now()
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Mock-Tokens", strconv.Itoa(tokens))
	req.Header.Set("X-Mock-TPS", strconv.Itoa(tps))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := c.Do(req)
	if err != nil {
		s.err = err.Error()
		return s
	}
	defer resp.Body.Close()
	s.status = resp.StatusCode
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		s.err = fmt.Sprintf("HTTP %d %s", resp.StatusCode, bytes.TrimSpace(b))
		return s
	}
	br := bufio.NewReaderSize(resp.Body, 32<<10)
	var first, last time.Time
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && bytes.HasPrefix(line, []byte("data: ")) {
			now := time.Now()
			if bytes.HasPrefix(line, []byte("data: [DONE]")) {
				s.finished = true
			} else if bytes.Contains(line, []byte(`"content"`)) {
				if first.IsZero() {
					first = now
					s.ttft = now.Sub(start)
				} else if g := now.Sub(last); g > s.maxGap {
					s.maxGap = g
				}
				last = now
				s.chunks++
			}
		}
		if err != nil {
			break
		}
	}
	s.total = time.Since(start)
	if !first.IsZero() && last.After(first) {
		s.rate = float64(s.chunks-1) / last.Sub(first).Seconds()
	}
	return s
}
