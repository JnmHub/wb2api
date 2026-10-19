package catalog

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"wb2api/internal/jsonx"
	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/upstream"
)

const (
	// ConfigPath is the desktop client's own product-config endpoint.
	ConfigPath = "/v3/config"

	cacheTTL        = 5 * time.Minute
	refreshCooldown = 30 * time.Second
	dailyInterval   = 24 * time.Hour
	maxConfigBody   = 8 << 20
)

// Source says where a published list came from.
const (
	SourceRemote  = "remote"  // upstream /v3/config
	SourceCache   = "cache"   // the desktop app's cache file on this machine
	SourceLegacy  = "legacy"  // same file, loose parse
	SourceBundled = "bundled" // the embedded snapshot only
)

// Meta is the provenance of the last list built for a realm.
type Meta struct {
	Source    string   `json:"source"`
	FetchedAt float64  `json:"fetched_at"`
	FileMtime *float64 `json:"file_mtime"`
}

type cached struct {
	at      time.Time
	entries []Entry
	meta    Meta
}

// Service owns the live catalogue of both exits.
type Service struct {
	Pool *pool.Pool
	Up   *upstream.Manager
	// Dir is the accounts directory (model.json and output_probes.json live
	// there); CacheDir receives an archive copy of the last /v3/config body.
	Dir      string
	CacheDir string
	// UpstreamOverride replaces the realm's chat upstream (tests, mock).
	UpstreamOverride string
	// Home is where the desktop apps keep their cache files (default: $HOME).
	Home string
	Log  *slog.Logger
	// ModelsDev enables the background models.dev limit refresh.
	ModelsDev bool
	// Now is the clock (tests).
	Now func() time.Time

	limOnce sync.Once
	lim     *limitIndex

	cache    sync.Map // realm -> *cached
	fetchMu  sync.Mutex
	rateMu   sync.Mutex
	lastTry  map[string]time.Time
	lastGood map[string]time.Time
	warned   atomic.Pointer[map[string]string]
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Service) limits() *limitIndex {
	s.limOnce.Do(func() { s.lim = &limitIndex{dir: s.Dir} })
	return s.lim
}

// ------------------------------------------------------------------ queries

// Models returns the published list for a realm ("intl", "cn" or "all"),
// rebuilding it from upstream when the 5-minute cache is stale. One rebuild
// runs at a time: a burst of /v1/models calls never becomes a burst of
// upstream calls.
func (s *Service) Models(ctx context.Context, r string) ([]Entry, Meta) {
	if c := s.fresh(r); c != nil {
		return c.entries, c.meta
	}
	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()
	if c := s.fresh(r); c != nil {
		return c.entries, c.meta
	}
	live, extras, source := s.curatedLive(ctx, r)
	entries := Merge(live, r, extras)
	if len(live) == 0 {
		source = SourceBundled
	}
	var mtime *float64
	if (source == SourceCache || source == SourceLegacy) && (r == realm.Intl || r == realm.CN) {
		if st, err := os.Stat(s.productConfigPath(r)); err == nil {
			t := float64(st.ModTime().UnixNano()) / 1e9
			mtime = &t
		}
	}
	c := &cached{at: s.now(), entries: entries,
		meta: Meta{Source: source, FetchedAt: float64(s.now().UnixNano()) / 1e9, FileMtime: mtime}}
	s.cache.Store(r, c)
	if source == SourceRemote {
		s.noteGood(r)
	}
	return c.entries, c.meta
}

func (s *Service) fresh(r string) *cached {
	if v, ok := s.cache.Load(r); ok {
		c := v.(*cached)
		if len(c.entries) > 0 && s.now().Sub(c.at) < cacheTTL {
			return c
		}
	}
	return nil
}

// metaOf is the catalogue metadata of one model, or nil. Like the legacy
// lookup it never touches the network: it reads the cached list, falling back
// to the bundled table.
func (s *Service) metaOf(model string) *jsonx.Obj {
	if model == "" {
		return nil
	}
	r := realm.DetectModelRealm(model, realm.Intl)
	var entries []Entry
	if v, ok := s.cache.Load(r); ok {
		entries = v.(*cached).entries
	}
	if len(entries) == 0 {
		entries = Static(r)
	}
	want := strings.ToLower(strings.TrimSpace(model))
	for _, e := range entries {
		if strings.ToLower(strings.TrimSpace(e.ID)) == want {
			return e.Meta
		}
	}
	return nil
}

// MaxOutputTokens is the catalogue's declared output ceiling (0 = unknown);
// together with DefaultEffort it makes *Service a shape.Catalog.
func (s *Service) MaxOutputTokens(model string) int {
	if m := s.metaOf(model); m != nil {
		if n := pyInt(m.Get("maxOutputTokens")); n > 0 {
			return int(n)
		}
	}
	return 0
}

// DefaultEffort is the reasoning effort applied when a client asks for none.
func (s *Service) DefaultEffort(model string) string {
	if m := s.metaOf(model); m != nil {
		if r, ok := m.Obj("reasoning"); ok {
			if e, ok := r.Str("defaultEffort"); ok {
				return strings.TrimSpace(e)
			}
		}
	}
	return ""
}

// Entries renders the /v1/models data array for a realm.
func (s *Service) Entries(ctx context.Context, r string) (data []*jsonx.Obj, meta Meta) {
	entries, meta := s.Models(ctx, r)
	if s.ModelsDev {
		s.limits().RefreshAsync(func(f string, a ...any) { s.logger().Debug(fmt.Sprintf(f, a...)) })
	}
	env := EntryEnv{Created: s.now().Unix(), Index: s.limits().Index(), Clamp: s.limits().Clamp}
	data = make([]*jsonx.Obj, 0, len(entries))
	for _, e := range entries {
		data = append(data, ModelEntry(e.ID, e.Meta, env))
	}
	return data, meta
}

// ------------------------------------------------------------------ sources

func (s *Service) home() string {
	if s.Home != "" {
		return s.Home
	}
	h, _ := os.UserHomeDir()
	return h
}

func (s *Service) productConfigPath(r string) string {
	dir := ".workbuddy"
	if r == realm.Intl {
		dir = ".workbuddy-ai"
	}
	return filepath.Join(s.home(), dir, "cache", "acc-product-config-v3.json")
}

func (s *Service) readCachedRemote(r string) *Parsed {
	if r == "all" {
		first, second := s.readCachedRemote(realm.Intl), s.readCachedRemote(realm.CN)
		if first == nil {
			return second
		}
		if second == nil {
			return first
		}
		seen := idSetStrings(first.IDs)
		ids := append([]string(nil), first.IDs...)
		for _, id := range second.IDs {
			if !seen[id] {
				ids = append(ids, id)
			}
		}
		meta := map[string]*jsonx.Obj{}
		for k, v := range second.Meta {
			meta[k] = v
		}
		for k, v := range first.Meta {
			meta[k] = v
		}
		return &Parsed{IDs: ids, Meta: meta}
	}
	raw, err := os.ReadFile(s.productConfigPath(r))
	if err != nil {
		return nil
	}
	doc, err := jsonx.Unmarshal(raw)
	if err != nil {
		return nil
	}
	return ParseRemote(doc)
}

// readLegacy is the loose parse of the same file: the first "models" array of
// objects with ids, wherever it sits.
func (s *Service) readLegacy(r string) []Entry {
	read := func(realmName string) []Entry {
		raw, err := os.ReadFile(s.productConfigPath(realmName))
		if err != nil {
			return nil
		}
		doc, err := jsonx.Unmarshal(raw)
		if err != nil {
			return nil
		}
		var find func(any) []any
		find = func(n any) []any {
			if o, ok := n.(*jsonx.Obj); ok {
				if ms, _ := o.Arr("models"); len(ms) > 0 {
					if first, ok := ms[0].(*jsonx.Obj); ok && jsonx.Truthy(first.Get("id")) {
						return ms
					}
				}
				var hit []any
				o.Each(func(_ string, v any) {
					if hit == nil {
						hit = find(v)
					}
				})
				return hit
			}
			return nil
		}
		var out []Entry
		for _, m := range find(doc) {
			if o, ok := m.(*jsonx.Obj); ok {
				if id, ok := o.Str("id"); ok && id != "" {
					out = append(out, Entry{ID: id, Meta: o})
				}
			}
		}
		return out
	}
	if r != "all" {
		return read(r)
	}
	out := read(realm.Intl)
	seen := map[string]bool{}
	for _, e := range out {
		seen[e.ID] = true
	}
	for _, e := range read(realm.CN) {
		if !seen[e.ID] {
			out = append(out, e)
		}
	}
	return out
}

// curatedLive returns the realm's live list: upstream first, then the desktop
// cache file, then its loose parse. extras says the batch came from the
// desktop catalogue and may add models the curated tables have never seen.
func (s *Service) curatedLive(ctx context.Context, r string) (live []Entry, extras bool, source string) {
	if p := s.fetchRemote(ctx, r); p != nil {
		return Live(Curate(p.IDs, p.Meta), p.Meta), true, SourceRemote
	}
	if p := s.readCachedRemote(r); p != nil {
		return Live(Curate(p.IDs, p.Meta), p.Meta), true, SourceCache
	}
	if legacy := s.readLegacy(r); len(legacy) > 0 {
		ids := make([]string, len(legacy))
		meta := map[string]*jsonx.Obj{}
		for i, e := range legacy {
			ids[i], meta[e.ID] = e.ID, e.Meta
		}
		keep := idSetStrings(Curate(ids, meta))
		var out []Entry
		for _, e := range legacy {
			if keep[e.ID] {
				out = append(out, e)
			}
		}
		return out, false, SourceLegacy
	}
	return nil, false, ""
}
