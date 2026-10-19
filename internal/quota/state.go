// Package quota keeps the model-quota clocks the upstream does not tell us
// about: the window probe (after a 429 with a reset clock, fire one minimal
// request at that instant to find out whether the line reopened) and the
// remaining-usage estimate. It is a port of legacy wb_window_probe.py and
// wb_quota_scan.py.
package quota

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// writeJSONAtomic replaces path with v, tolerating a missing directory.
func writeJSONAtomic(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readJSONFile reads a JSON file leniently: a BOM and a missing/corrupt file
// all mean "empty state", never an error the caller has to handle.
func readJSONFile(path string, v any) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	raw = []byte(strings.TrimPrefix(string(raw), "\ufeff"))
	if err := json.Unmarshal(raw, v); err != nil {
		return false
	}
	return true
}

// stamp formats an instant the way the legacy state files do.
func stamp(t time.Time) string { return t.Format("2006-01-02 15:04:05") }

// clamp truncates a detail string to n bytes (UTF-8 is repaired by the caller's
// string slicing being byte-based, which matches Python's [:200] on ASCII logs).
func clamp(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
