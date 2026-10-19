package catalog

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	modelsDevURL      = "https://models.dev/api.json"
	modelsDevTimeout  = 5 * time.Second
	modelsDevCooldown = 5 * time.Minute
	modelsDevMaxBody  = 32 << 20
	modelsDevValueMax = 1_000_000_000
	limitCacheFile    = "model.json"
	probeFile         = "output_probes.json"
	probeTTL          = time.Minute
)

// vendorSources win over aggregator gateways when models.dev disagrees.
var vendorSources = map[string]bool{
	"zai": true, "moonshotai": true, "moonshotai-cn": true, "openai": true,
	"google": true, "deepseek": true, "minimax": true,
}

// limitIndex is the local models.dev index: bare id -> {context, output}.
type limitIndex struct {
	dir string

	idx      atomic.Pointer[map[string][2]int64]
	mu       sync.Mutex
	lastTry  time.Time
	fetching bool

	probes atomic.Pointer[probeSet]
}

type probeSet struct {
	at   time.Time
	data map[string]int64
}

func (l *limitIndex) path() string { return filepath.Join(l.dir, limitCacheFile) }

// Index returns the local index (loading model.json on first use). The result
// must not be modified.
func (l *limitIndex) Index() map[string][2]int64 {
	if p := l.idx.Load(); p != nil {
		return *p
	}
	m := loadLimitFile(l.path())
	l.idx.Store(&m)
	return m
}

func loadLimitFile(path string) map[string][2]int64 {
	out := map[string][2]int64{}
	raw, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil {
		return out
	}
	for k, v := range doc {
		var pair []json.Number
		var obj struct {
			Context json.Number `json:"context"`
			Output  json.Number `json:"output"`
		}
		var c, o int64
		if json.Unmarshal(v, &pair) == nil && len(pair) == 2 {
			c, o = pyInt(pair[0]), pyInt(pair[1])
		} else if json.Unmarshal(v, &obj) == nil {
			c, o = pyInt(obj.Context), pyInt(obj.Output)
		}
		if c > 0 || o > 0 {
			out[BareID(k)] = [2]int64{c, o}
		}
	}
	return out
}

// buildIndex turns a models.dev api.json document into the limit index.
// Vendor-official providers win; otherwise the most common pair wins (ties go
// to the smaller one). Invalid or absurd values are dropped.
func buildIndex(doc map[string]struct {
	Models map[string]struct {
		Limit struct {
			Context json.Number `json:"context"`
			Output  json.Number `json:"output"`
		} `json:"limit"`
	} `json:"models"`
}) map[string][2]int64 {
	type vote struct {
		values map[[2]int64]int
		vendor *[2]int64
	}
	votes := map[string]*vote{}
	// Providers are visited in sorted order so the first vendor hit is stable.
	provs := make([]string, 0, len(doc))
	for p := range doc {
		provs = append(provs, p)
	}
	sort.Strings(provs)
	for _, p := range provs {
		models := make([]string, 0, len(doc[p].Models))
		for id := range doc[p].Models {
			models = append(models, id)
		}
		sort.Strings(models)
		for _, id := range models {
			lim := doc[p].Models[id].Limit
			c, o := pyInt(lim.Context), pyInt(lim.Output)
			if (c <= 0 && o <= 0) || c > modelsDevValueMax || o > modelsDevValueMax {
				continue
			}
			key := BareID(id)
			v := votes[key]
			if v == nil {
				v = &vote{values: map[[2]int64]int{}}
				votes[key] = v
			}
			pair := [2]int64{c, o}
			v.values[pair]++
			if vendorSources[p] && v.vendor == nil {
				cp := pair
				v.vendor = &cp
			}
		}
	}
	out := make(map[string][2]int64, len(votes))
	for key, v := range votes {
		if v.vendor != nil {
			out[key] = *v.vendor
			continue
		}
		best, bestN := [2]int64{}, 0
		for pair, n := range v.values {
			if n > bestN || (n == bestN && (pair[0] < best[0] || (pair[0] == best[0] && pair[1] < best[1]))) {
				best, bestN = pair, n
			}
		}
		if bestN > 0 {
			out[key] = best
		}
	}
	return out
}

// RefreshAsync warms the local index from models.dev in the background
// (throttled, offline-safe). It reports whether a fetch was started.
func (l *limitIndex) RefreshAsync(logf func(string, ...any)) bool {
	l.mu.Lock()
	if l.fetching || time.Since(l.lastTry) < modelsDevCooldown {
		l.mu.Unlock()
		return false
	}
	l.fetching, l.lastTry = true, time.Now()
	l.mu.Unlock()

	go func() {
		defer func() { l.mu.Lock(); l.fetching = false; l.mu.Unlock() }()
		client := &http.Client{Timeout: modelsDevTimeout}
		req, _ := http.NewRequest(http.MethodGet, modelsDevURL, nil)
		req.Header.Set("User-Agent", "wb2api-hub")
		resp, err := client.Do(req)
		if err != nil {
			logf("models.dev refresh skipped: %v", err)
			return
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, modelsDevMaxBody))
		if err != nil || resp.StatusCode != http.StatusOK {
			logf("models.dev refresh skipped: status %d err %v", resp.StatusCode, err)
			return
		}
		l.ingest(raw, logf)
	}()
	return true
}

func (l *limitIndex) ingest(raw []byte, logf func(string, ...any)) {
	var doc map[string]struct {
		Models map[string]struct {
			Limit struct {
				Context json.Number `json:"context"`
				Output  json.Number `json:"output"`
			} `json:"limit"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		logf("models.dev refresh skipped: %v", err)
		return
	}
	idx := buildIndex(doc)
	if len(idx) == 0 {
		return
	}
	l.idx.Store(&idx)
	file := make(map[string][]int64, len(idx))
	for k, v := range idx {
		file[k] = []int64{v[0], v[1]}
	}
	if b, err := json.Marshal(file); err == nil {
		if os.MkdirAll(l.dir, 0o755) == nil {
			tmp := l.path() + ".tmp"
			if os.WriteFile(tmp, b, 0o644) == nil {
				_ = os.Rename(tmp, l.path())
			}
		}
	}
}

// Clamp returns the measured upstream output clamp of a model (0 = unknown),
// read from output_probes.json with a one-minute cache.
func (l *limitIndex) Clamp(model string) int64 {
	ps := l.probes.Load()
	if ps == nil || time.Since(ps.at) > probeTTL {
		ps = &probeSet{at: time.Now(), data: map[string]int64{}}
		if raw, err := os.ReadFile(filepath.Join(l.dir, probeFile)); err == nil {
			var doc map[string]json.RawMessage
			if json.Unmarshal(raw, &doc) == nil {
				for k, v := range doc {
					var e struct {
						Clamped json.Number `json:"clamped"`
					}
					if json.Unmarshal(v, &e) == nil {
						if n := pyInt(e.Clamped); n > 0 {
							ps.data[k] = n
						}
					}
				}
			}
		}
		l.probes.Store(ps)
	}
	return ps.data[model]
}
