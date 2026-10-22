// Package pricing estimates API-equivalent token cost from OpenRouter's listed
// prices (USD per million tokens), converted to CNY at a fixed rate. It is a
// trimmed port of legacy wb_pricing.py: flat prices, conditional bands, the
// per-model timeline and the policy sweep are not ported. A model can be mapped
// by hand or priced with explicit overrides.
package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	openRouterModels = "https://openrouter.ai/api/v1/models"
	defaultUSDCNY    = 7.1
	maxModels        = 20000
)

// Price is USD per million tokens.
type Price struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// Model is one priced OpenRouter entry.
type Model struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Price Price  `json:"price"`
}

// Snapshot is the cached OpenRouter catalogue.
type Snapshot struct {
	FetchedAt string           `json:"fetched_at"`
	USDCNY    float64          `json:"usd_cny"`
	Source    string           `json:"source"`
	Models    map[string]Model `json:"models"`
	Error     string           `json:"error,omitempty"`
}

// Policy overrides how one gateway model is priced.
type Policy struct {
	Mode       string   `json:"mode"` // auto | manual | ignore
	ORID       string   `json:"or_id,omitempty"`
	Input      *float64 `json:"input,omitempty"`
	Output     *float64 `json:"output,omitempty"`
	CacheRead  *float64 `json:"cache_read,omitempty"`
	CacheWrite *float64 `json:"cache_write,omitempty"`
	Note       string   `json:"note,omitempty"`
}

// PolicySource persists the per-model overrides outside the local file. A
// PostgreSQL-backed source lets every replica see the same prices.
type PolicySource interface {
	LoadPolicies() (map[string]Policy, error)
	SavePolicies(map[string]Policy) error
}

// Store owns the snapshot and the per-model policies under one data directory.
type Store struct {
	Dir      string
	Client   *http.Client
	FetchURL string

	mu            sync.RWMutex
	snap          Snapshot
	policies      map[string]Policy
	policySource  PolicySource
	policyVersion atomic.Uint64
	loaded        bool
	lastErr       string
	now           func() time.Time
}

// Open loads the snapshot and policies (missing files are fine).
func Open(dir string) *Store {
	s := &Store{Dir: dir, FetchURL: openRouterModels, policies: map[string]Policy{}, now: time.Now}
	s.Client = &http.Client{Timeout: 30 * time.Second}
	s.load()
	return s
}

func (s *Store) snapshotPath() string { return filepath.Join(s.Dir, "pricing.json") }
func (s *Store) policyPath() string   { return filepath.Join(s.Dir, "pricing_policies.json") }

// UsePolicySource loads policies from a shared source and makes later writes go
// there. An empty shared set is seeded from the local file so an upgrade keeps
// the existing overrides.
func (s *Store) UsePolicySource(src PolicySource) error {
	if src == nil {
		return nil
	}
	shared, err := src.LoadPolicies()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if shared == nil {
		shared = map[string]Policy{}
	}
	if len(shared) == 0 && len(s.policies) > 0 {
		if err := src.SavePolicies(copyPolicies(s.policies)); err != nil {
			return err
		}
		shared = copyPolicies(s.policies)
	}
	s.policies = shared
	s.policySource = src
	s.policyVersion.Add(1)
	return nil
}

// StartPolicySync polls the shared policy source so a change made on another
// replica becomes visible here. A local write racing with the poll wins.
func (s *Store) StartPolicySync(ctx context.Context, interval time.Duration) <-chan struct{} {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.syncPolicies()
			}
		}
	}()
	return done
}

func (s *Store) syncPolicies() {
	s.mu.RLock()
	src := s.policySource
	s.mu.RUnlock()
	if src == nil {
		return
	}
	before := s.policyVersion.Load()
	shared, err := src.LoadPolicies()
	if err != nil {
		s.setErr(err)
		return
	}
	if shared == nil {
		shared = map[string]Policy{}
	}
	s.mu.Lock()
	if s.policyVersion.Load() != before {
		s.mu.Unlock() // a panel write landed while we were reading
		return
	}
	s.policies = shared
	s.mu.Unlock()
}

func copyPolicies(in map[string]Policy) map[string]Policy {
	out := make(map[string]Policy, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (s *Store) load() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if raw, err := os.ReadFile(s.snapshotPath()); err == nil {
		var snap Snapshot
		if json.Unmarshal(raw, &snap) == nil {
			if snap.USDCNY <= 0 {
				snap.USDCNY = defaultUSDCNY
			}
			if snap.Models == nil {
				snap.Models = map[string]Model{}
			}
			s.snap = snap
		}
	}
	if raw, err := os.ReadFile(s.policyPath()); err == nil {
		var doc struct {
			Policies map[string]Policy `json:"policies"`
		}
		if json.Unmarshal(raw, &doc) == nil && doc.Policies != nil {
			s.policies = doc.Policies
		}
	}
	s.loaded = true
}

func writeAtomic(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Snapshot returns a copy of the cached catalogue.
func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap
}

// Policies returns a copy keyed by gateway model id.
func (s *Store) Policies() map[string]Policy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Policy, len(s.policies))
	for k, v := range s.policies {
		out[k] = v
	}
	return out
}

// Refresh fetches OpenRouter's catalogue and stores it.
func (s *Store) Refresh(ctx context.Context) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.FetchURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "wb2api")
	resp, err := s.Client.Do(req)
	if err != nil {
		s.setErr(err)
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode/100 != 2 {
		err := fmt.Errorf("openrouter HTTP %d", resp.StatusCode)
		s.setErr(err)
		return 0, err
	}
	var doc struct {
		Data []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Pricing struct {
				Prompt         string `json:"prompt"`
				Completion     string `json:"completion"`
				InputCacheRead string `json:"input_cache_read"`
				CacheWrite     string `json:"input_cache_write"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		s.setErr(err)
		return 0, fmt.Errorf("openrouter response: %w", err)
	}
	models := make(map[string]Model, len(doc.Data))
	for _, m := range doc.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		price := Price{
			Input:      perMillion(m.Pricing.Prompt),
			Output:     perMillion(m.Pricing.Completion),
			CacheRead:  perMillion(m.Pricing.InputCacheRead),
			CacheWrite: perMillion(m.Pricing.CacheWrite),
		}
		if price.Input == 0 && price.Output == 0 {
			continue
		}
		if price.CacheRead == 0 {
			price.CacheRead = price.Input
		}
		models[id] = Model{ID: id, Name: m.Name, Price: price}
		if len(models) >= maxModels {
			break
		}
	}
	if len(models) == 0 {
		err := fmt.Errorf("openrouter returned no priced models")
		s.setErr(err)
		return 0, err
	}
	snap := Snapshot{FetchedAt: s.now().UTC().Format(time.RFC3339), USDCNY: defaultUSDCNY,
		Source: s.FetchURL, Models: models}
	if err := writeAtomic(s.snapshotPath(), snap); err != nil {
		s.setErr(err)
		return 0, err
	}
	s.mu.Lock()
	s.snap = snap
	s.lastErr = ""
	s.mu.Unlock()
	return len(models), nil
}

func perMillion(v string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || f < 0 {
		return 0
	}
	return f * 1_000_000
}

func (s *Store) setErr(err error) {
	s.mu.Lock()
	s.lastErr = err.Error()
	s.mu.Unlock()
}

// SetPolicy stores (or clears, with an empty mode) one model policy.
func (s *Store) SetPolicy(model string, p Policy) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return fmt.Errorf("model is required")
	}
	switch p.Mode {
	case "auto":
		p.ORID, p.Input, p.Output, p.CacheRead, p.CacheWrite = "", nil, nil, nil, nil
	case "manual":
		if p.ORID == "" && p.Input == nil && p.Output == nil {
			return fmt.Errorf("manual pricing needs an OpenRouter id or explicit prices")
		}
		if p.Input != nil && *p.Input < 0 || p.Output != nil && *p.Output < 0 ||
			p.CacheRead != nil && *p.CacheRead < 0 || p.CacheWrite != nil && *p.CacheWrite < 0 {
			return fmt.Errorf("prices cannot be negative")
		}
	case "ignore":
		p.ORID, p.Input, p.Output, p.CacheRead, p.CacheWrite = "", nil, nil, nil, nil
	case "":
		s.mu.Lock()
		prev, had := s.policies[model]
		delete(s.policies, model)
		s.policyVersion.Add(1)
		err := s.savePoliciesLocked()
		if err != nil && had {
			s.policies[model] = prev
		}
		s.mu.Unlock()
		return err
	default:
		return fmt.Errorf("mode must be auto, manual or ignore")
	}
	s.mu.Lock()
	prev, had := s.policies[model]
	s.policies[model] = p
	s.policyVersion.Add(1)
	err := s.savePoliciesLocked()
	if err != nil {
		if had {
			s.policies[model] = prev
		} else {
			delete(s.policies, model)
		}
	}
	s.mu.Unlock()
	return err
}

func (s *Store) savePoliciesLocked() error {
	if s.policySource != nil {
		return s.policySource.SavePolicies(copyPolicies(s.policies))
	}
	return writeAtomic(s.policyPath(), map[string]any{"policies": s.policies})
}

// Price resolves a gateway model id into a price. source is one of
// "policy", "exact", "alias", "suffix".
func (s *Store) Price(model string) (Price, string, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return Price{}, "", false
	}
	s.mu.RLock()
	p, hasPolicy := s.policies[model]
	snap := s.snap
	s.mu.RUnlock()
	if hasPolicy && p.Mode == "ignore" {
		return Price{}, "", false
	}
	if hasPolicy && p.Mode == "manual" {
		price := Price{}
		if p.Input != nil {
			price.Input = *p.Input
		}
		if p.Output != nil {
			price.Output = *p.Output
		}
		if p.CacheRead != nil {
			price.CacheRead = *p.CacheRead
		}
		if p.CacheWrite != nil {
			price.CacheWrite = *p.CacheWrite
		}
		if p.ORID != "" {
			if m, ok := lookup(snap.Models, p.ORID); ok {
				if p.Input == nil {
					price.Input = m.Price.Input
				}
				if p.Output == nil {
					price.Output = m.Price.Output
				}
				if p.CacheRead == nil {
					price.CacheRead = m.Price.CacheRead
				}
				if p.CacheWrite == nil {
					price.CacheWrite = m.Price.CacheWrite
				}
			}
		}
		if price.CacheRead == 0 {
			price.CacheRead = price.Input
		}
		return price, "policy", price.Input > 0 || price.Output > 0
	}
	orID := ""
	if hasPolicy {
		orID = p.ORID
	}
	if orID != "" {
		if m, ok := lookup(snap.Models, orID); ok {
			return m.Price, "policy", true
		}
	}
	if m, ok := lookup(snap.Models, model); ok {
		return m.Price, "exact", true
	}
	// Gateway names usually drop the provider prefix: match a unique slug.
	var alias *Model
	for id, m := range snap.Models {
		slug := id
		if i := strings.LastIndex(id, "/"); i >= 0 {
			slug = id[i+1:]
		}
		if strings.EqualFold(slug, model) {
			if alias != nil {
				alias = nil
				break
			}
			copy := m
			alias = &copy
		}
	}
	if alias != nil {
		return alias.Price, "alias", true
	}
	// Channel-suffixed names inherit from a base that matches uniquely.
	parts := strings.Split(model, "-")
	for i := len(parts) - 1; i >= 2; i-- {
		base := strings.Join(parts[:i], "-")
		if m, ok := lookup(snap.Models, base); ok {
			return m.Price, "suffix", true
		}
		if alias := uniqueSlug(snap.Models, base); alias != nil {
			return alias.Price, "suffix", true
		}
	}
	return Price{}, "", false
}

func lookup(models map[string]Model, id string) (Model, bool) {
	if m, ok := models[id]; ok {
		return m, true
	}
	for k, m := range models {
		if strings.EqualFold(k, id) {
			return m, true
		}
	}
	return Model{}, false
}

func uniqueSlug(models map[string]Model, name string) *Model {
	var found *Model
	for id, m := range models {
		slug := id
		if i := strings.LastIndex(id, "/"); i >= 0 {
			slug = id[i+1:]
		}
		if strings.EqualFold(slug, name) {
			if found != nil {
				return nil
			}
			copy := m
			found = &copy
		}
	}
	return found
}

// Cost prices one usage row. unix time is accepted for API symmetry though the
// trimmed port uses one flat price per model.
func (s *Store) Cost(model string, prompt, completion, cached int64, unix int64) (cny, usd float64, ok bool) {
	price, _, ok := s.Price(model)
	if !ok {
		return 0, 0, false
	}
	if cached < 0 {
		cached = 0
	}
	if cached > prompt {
		cached = prompt
	}
	miss := prompt - cached
	u := (float64(miss)*price.Input + float64(cached)*price.CacheRead + float64(completion)*price.Output) / 1_000_000
	rate := s.snap.USDCNY
	if rate <= 0 {
		rate = defaultUSDCNY
	}
	return u * rate, u, true
}

// Status is the pricing panel payload.
func (s *Store) Status() map[string]any {
	s.mu.RLock()
	snap := s.snap
	errText := s.lastErr
	s.mu.RUnlock()
	policies := s.Policies()
	models := make([]Model, 0, len(snap.Models))
	for _, m := range snap.Models {
		models = append(models, m)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return map[string]any{
		"fetched_at": snap.FetchedAt, "usd_cny": snap.USDCNY, "source": snap.Source,
		"models": len(models), "catalogue": models, "policies": policies, "error": errText,
		"stale": s.stale(),
	}
}

func (s *Store) stale() bool {
	s.mu.RLock()
	fetched := s.snap.FetchedAt
	s.mu.RUnlock()
	if fetched == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, fetched)
	if err != nil {
		return true
	}
	return s.now().Sub(t) > 24*time.Hour
}
