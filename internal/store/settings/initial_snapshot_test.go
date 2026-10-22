package settings_test

import (
	"errors"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/gateway/openai"
	"wb2api/internal/store/settings"
)

func TestInitialBackendSnapshotRetainsAuthenticationThroughOutage(t *testing.T) {
	initialKey := map[string]any{"id": "known", "key": "sk-known", "enabled": true, "models": []any{"allowed*"}}
	initialRows := []any{initialKey}
	initial := settings.Doc{"api_keys": initialRows, "auth_disabled": false, "unknown_future": map[string]any{"keep": true}}
	var loads atomic.Int32
	s := settings.OpenWithUpdaterInitial(initial, func() (settings.Doc, error) {
		loads.Add(1)
		return nil, errors.New("backend unavailable after successful startup load")
	}, nil, func(fn func(settings.Doc) error) (settings.Doc, error) {
		// Transactional update does its own read and returns its committed document.
		committed := settings.Doc{"api_keys": []any{map[string]any{"id": "known", "key": "sk-known", "enabled": true}}}
		if err := fn(committed); err != nil {
			return nil, err
		}
		return committed, nil
	})
	// Construction must detach both nested objects and array storage from the
	// caller, not merely clone the top-level document containing API-key policy.
	initial["auth_disabled"] = true
	initialKey["key"] = "sk-mutated"
	initialKey["models"].([]any)[0] = "denied*"
	initialRows[0] = map[string]any{"key": "sk-replaced"}
	initial["unknown_future"].(map[string]any)["keep"] = false
	if got := s.View()["unknown_future"].(map[string]any)["keep"]; got != true {
		t.Fatalf("initial nested map aliased: %v", got)
	}
	rev := s.Rev()
	if rev == 0 {
		t.Fatal("initial snapshot missing revision")
	}
	h := &openai.Handler{Settings: s}
	assertAuth := func(secret string, want bool) {
		t.Helper()
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		if secret != "" {
			r.Header.Set("Authorization", "Bearer "+secret)
		}
		if _, ok := h.Authenticate(r); ok != want {
			t.Fatalf("Authenticate(%q)=%v want %v", secret, ok, want)
		}
	}
	if !h.KeyRequired() {
		t.Fatal("known initial keys must require auth")
	}
	assertAuth("", false)
	assertAuth("sk-known", true)
	assertAuth("sk-mutated", false)
	keys := s.APIKeys(false)
	if len(keys) != 1 || len(keys[0].Models) != 1 || keys[0].Models[0] != "allowed*" {
		t.Fatalf("initial nested array aliased: %+v", keys)
	}
	if got := loads.Load(); got != 0 {
		t.Fatalf("first View/auth reread successful startup document: loads=%d", got)
	}
	// Once polling is due, failure must retain the real snapshot and revision,
	// never install a cold empty document that would disable API-key checks.
	time.Sleep(settings.CheckInterval + 20*time.Millisecond)
	if got := s.Rev(); got != rev {
		t.Fatalf("outage changed known-good revision: got %d want %d", got, rev)
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("due outage poll count=%d", got)
	}
	if !h.KeyRequired() {
		t.Fatal("backend outage disabled API-key authentication")
	}
	assertAuth("", false)
	assertAuth("sk-known", true)
	if err := s.Update(func(d settings.Doc) error {
		d["api_keys"].([]any)[0].(map[string]any)["key"] = "sk-rotated"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := s.Rev(); got <= rev {
		t.Fatalf("committed update did not advance revision: %d <= %d", got, rev)
	}
	assertAuth("sk-known", false)
	assertAuth("sk-rotated", true)
	assertAuth("", false)
	if got := loads.Load(); got != 1 {
		t.Fatalf("committed update unnecessarily reread outage backend: %d", got)
	}
}
