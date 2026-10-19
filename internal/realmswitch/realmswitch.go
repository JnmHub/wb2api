// Package realmswitch holds the panel's "active exit" switch: the realm used
// when neither the API key, an X-Realm header nor a ?realm= argument picks one.
// It is persisted next to the accounts so a restart keeps the choice.
package realmswitch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// Switch is the active realm.
type Switch struct {
	path string
	cur  atomic.Value // string
}

// Open loads the persisted realm (falling back to def, then "intl").
func Open(accountsDir, def string) *Switch {
	s := &Switch{path: filepath.Join(accountsDir, "active_realm.json")}
	if def != "intl" && def != "cn" {
		def = "intl"
	}
	s.cur.Store(def)
	if b, err := os.ReadFile(s.path); err == nil {
		var d struct {
			Realm string `json:"realm"`
		}
		if json.Unmarshal(b, &d) == nil && (d.Realm == "intl" || d.Realm == "cn") {
			s.cur.Store(d.Realm)
		}
	}
	return s
}

// Get is the active realm.
func (s *Switch) Get() string {
	if s == nil {
		return "intl"
	}
	return s.cur.Load().(string)
}

// Set switches and persists; unknown values are refused.
func (s *Switch) Set(realm string) bool {
	if realm != "intl" && realm != "cn" {
		return false
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
