package pool

import (
	"sort"
	"sync"
	"time"
)

// RemainingWeight is an advisory preference, never an account eligibility guard.
type RemainingWeight struct {
	Realm, Model, UID string
	Weight            int
}

type remainingKey struct{ realm, model string }
type remainingEntry struct {
	uid    string
	weight int
}
type remainingGroup struct {
	entries []remainingEntry // immutable after construction
	weights map[string]int
	mu      sync.Mutex // only smooth-WRR counters are mutable, isolated per realm/model
	current []int64
}

// RemainingSnapshot owns immutable per-realm/model scheduling weights. Callers
// cannot mutate the maps; dispatch reads never copy them. Revision identifies a
// publication, not an authorization epoch. A nil snapshot means plain rotation.
type RemainingSnapshot struct {
	revision uint64
	groups   map[remainingKey]*remainingGroup
}

// NewRemainingSnapshot copies weights once, outside dispatch. Empty Realm is a
// compatibility wildcard used only by ApplyRemainingWeights.
func NewRemainingSnapshot(revision uint64, weights []RemainingWeight) *RemainingSnapshot {
	s := &RemainingSnapshot{revision: revision, groups: make(map[remainingKey]*remainingGroup)}
	for _, w := range weights {
		if w.Weight <= 1 || w.UID == "" {
			continue
		}
		k := remainingKey{w.Realm, w.Model}
		g := s.groups[k]
		if g == nil {
			g = &remainingGroup{weights: make(map[string]int)}
			s.groups[k] = g
		}
		g.weights[w.UID] = w.Weight
	}
	for _, g := range s.groups {
		for uid, weight := range g.weights {
			g.entries = append(g.entries, remainingEntry{uid, weight})
		}
		sort.Slice(g.entries, func(i, j int) bool { return g.entries[i].uid < g.entries[j].uid })
		g.current = make([]int64, len(g.entries))
	}
	return s
}

func (s *RemainingSnapshot) Revision() uint64 {
	if s == nil {
		return 0
	}
	return s.revision
}
func (s *RemainingSnapshot) group(realm, model string) *remainingGroup {
	if s == nil {
		return nil
	}
	if g := s.groups[remainingKey{realm, model}]; g != nil {
		return g
	}
	return s.groups[remainingKey{"", model}]
}
func (s *RemainingSnapshot) Weight(realm, model, uid string) int {
	if g := s.group(realm, model); g != nil {
		if w := g.weights[uid]; w > 1 {
			return w
		}
	}
	return 1
}

// ApplyRemainingSnapshot publishes already-built weights without allocation or
// locking. Reapplying the same snapshot preserves smooth-WRR state.
func (p *Pool) ApplyRemainingSnapshot(s *RemainingSnapshot) { p.remainSnapshot.Store(s) }

func (p *Pool) pickRemaining(realm, model string, exclude map[string]struct{}, page *Page, prefs *realmPrefs, now time.Time) *Account {
	g := p.remainSnapshot.Load().group(realm, model)
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	snap := p.snap.Load()
	var best *Account
	bestIndex := -1
	var total int64
	for i, entry := range g.entries {
		a := snap.byUID[entry.uid]
		if a == nil || a.Realm != realm {
			g.current[i] = 0
			continue
		}
		if _, skip := exclude[a.UID]; skip {
			g.current[i] = 0
			continue
		}
		if page != nil && prefs != nil {
			if rank, ok := prefs.rank[a.UID]; !ok || rank%page.Size != page.Index%page.Size {
				g.current[i] = 0
				continue
			}
		}
		if !a.Ready(model, now) {
			g.current[i] = 0
			continue
		}
		g.current[i] += int64(entry.weight)
		total += int64(entry.weight)
		if bestIndex < 0 || g.current[i] > g.current[bestIndex] {
			best, bestIndex = a, i
		}
	}
	if bestIndex >= 0 {
		g.current[bestIndex] -= total
	}
	return best
}
