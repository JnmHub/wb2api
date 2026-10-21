// Package realmswitch holds the panel's "active exit" switch: the realm used
// when neither the API key, an X-Realm header nor a ?realm= argument picks one.
// It is persisted next to the accounts so a restart keeps the choice. When the
// panel runs against PostgreSQL the same value is stored in wb_state, so every
// replica sees the switch within one second.
package realmswitch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// RefreshInterval bounds how often a database-backed switch re-reads its value.
// The panel only switches a realm interactively, so a one-second propagation
// delay is invisible to users and the request path does at most one small
// indexed read per second, not one per request.
const RefreshInterval = time.Second

// Switch is the active realm.
type Switch struct {
	path      string
	cur       atomic.Value // string
	loadFn    func() (string, error)
	saveFn    func(string) error
	interval  time.Duration
	lastCheck atomic.Int64 // unix nanos of the last backend read
}

// Open loads the persisted realm (falling back to def, then "intl").
func Open(accountsDir, def string) *Switch {
	s := &Switch{path: filepath.Join(accountsDir, "active_realm.json")}
	s.cur.Store(normalize(def))
	if b, err := os.ReadFile(s.path); err == nil {
		var d struct {
			Realm string `json:"realm"`
		}
		if json.Unmarshal(b, &d) == nil && valid(d.Realm) {
			s.cur.Store(d.Realm)
		}
	}
	return s
}

// OpenWith returns a switch backed by external load/save functions (for
// example PostgreSQL). load is polled at most once per RefreshInterval, so a
// switch made on another replica becomes visible without a restart.
func OpenWith(load func() (string, error), save func(string) error, def string) *Switch {
	s := &Switch{loadFn: load, saveFn: save}
	s.cur.Store(normalize(def))
	if load != nil {
		if realm, err := load(); err == nil && valid(realm) {
			s.cur.Store(realm)
		}
	}
	return s
}

// Get is the active realm.
func (s *Switch) Get() string {
	if s == nil {
		return "intl"
	}
	if s.loadFn != nil {
		s.refresh()
	}
	return s.cur.Load().(string)
}

func (s *Switch) refresh() {
	now := time.Now().UnixNano()
	interval := s.interval
	if interval <= 0 {
		interval = RefreshInterval
	}
	last := s.lastCheck.Load()
	if last != 0 && now-last < int64(interval) {
		return
	}
	if !s.lastCheck.CompareAndSwap(last, now) {
		return
	}
	realm, err := s.loadFn()
	if err != nil || !valid(realm) {
		return
	}
	s.cur.Store(realm)
}

// Set switches and persists; unknown values are refused.
func (s *Switch) Set(realm string) bool {
	if !valid(realm) {
		return false
	}
	if s.saveFn != nil {
		if err := s.saveFn(realm); err != nil {
			return false
		}
		s.cur.Store(realm)
		s.lastCheck.Store(time.Now().UnixNano())
		return true
	}
	s.cur.Store(realm)
	_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
	b, _ := json.MarshalIndent(map[string]any{"realm": realm, "updated_at": float64(time.Now().UnixNano()) / 1e9,
		"updated_iso": time.Now().Format("2006-01-02 15:04:05")}, "", "  ")
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, s.path)
	}
	return true
}

func valid(realm string) bool { return realm == "intl" || realm == "cn" }

func normalize(realm string) string {
	if valid(realm) {
		return realm
	}
	return "intl"
}
