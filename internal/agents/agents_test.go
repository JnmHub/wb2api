package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

func newMgr(t *testing.T) (*Manager, string) {
	t.Helper()
	home := t.TempDir()
	dir := t.TempDir()
	m := New(dir)
	m.Home = home
	return m, home
}

func TestApplyAllClientsAndRestore(t *testing.T) {
	for _, c := range clients {
		t.Run(c.id, func(t *testing.T) {
			m, home := newMgr(t)
			paths := c.paths(home)
			// Pre-create one file with user content where the client supports it.
			var original []byte
			for _, p := range paths {
				if c.id == "claude-code" || c.id == "opencode" {
					original = []byte(`{"keep":true}`)
					_ = os.MkdirAll(filepath.Dir(p), 0o700)
					if err := os.WriteFile(p, original, 0o600); err != nil {
						t.Fatal(err)
					}
					break
				}
			}
			if _, err := m.Apply(c.id, "http://127.0.0.1:8787", "sk-test", "model-x", []string{"model-x"}); err != nil {
				t.Fatal(err)
			}
			views := m.Overview()
			var view *ClientView
			for i := range views {
				if views[i].ID == c.id {
					view = &views[i]
				}
			}
			if view == nil || !view.Configured || !view.Installed || view.Applied == nil || view.Applied.External {
				t.Fatalf("overview: %+v", view)
			}
			// The files parse and mention the provider.
			for key, p := range paths {
				raw, err := os.ReadFile(p)
				if err != nil {
					t.Fatalf("%s: %v", key, err)
				}
				if !strings.Contains(string(raw), ProviderID) && !strings.Contains(string(raw), "ANTHROPIC_AUTH_TOKEN") &&
					!strings.Contains(string(raw), "OPENAI_API_KEY") && !strings.Contains(string(raw), CredentialRef) {
					t.Fatalf("%s lacks the provider: %s", key, raw)
				}
				switch {
				case strings.HasSuffix(p, ".json"):
					var m map[string]any
					if err := json.Unmarshal(raw, &m); err != nil {
						t.Fatalf("%s: %v", p, err)
					}
				case strings.HasSuffix(p, ".toml"):
					var m map[string]any
					if err := toml.Unmarshal(raw, &m); err != nil {
						t.Fatalf("%s: %v", p, err)
					}
				case strings.HasSuffix(p, ".yaml"):
					var m map[string]any
					if err := yaml.Unmarshal(raw, &m); err != nil {
						t.Fatalf("%s: %v", p, err)
					}
				}
			}
			// A hand edit reads as an external change.
			first := ""
			for _, p := range paths {
				first = p
				break
			}
			_ = os.WriteFile(first, []byte("{}"), 0o600)
			for _, v := range m.Overview() {
				if v.ID == c.id && (v.Applied == nil || !v.Applied.External) {
					t.Fatalf("external change not detected: %+v", v.Applied)
				}
			}
			if _, err := m.Restore(c.id); err != nil {
				t.Fatal(err)
			}
			if original != nil {
				// The first file is restored byte for byte, the other deleted.
				if raw, _ := os.ReadFile(first); string(raw) != string(original) {
					t.Fatalf("restored %q, want %q", raw, original)
				}
			} else if _, err := os.Stat(first); !os.IsNotExist(err) {
				t.Fatalf("created file must be deleted: %v", err)
			}
			if _, err := m.Restore(c.id); err == nil {
				t.Fatal("a second restore must fail")
			}
		})
	}
}

func TestApplyKeepsFirstBackupAcrossApplies(t *testing.T) {
	m, home := newMgr(t)
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"env":{"KEEP":"1"}}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Apply("claude-code", "http://a", "k1", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Apply("claude-code", "http://b", "k2", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Restore("claude-code"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != string(original) {
		t.Fatalf("restore must return the pre-first-apply bytes, got %s", raw)
	}
}

func TestApplyValidation(t *testing.T) {
	m, _ := newMgr(t)
	if _, err := m.Apply("nope", "http://x", "k", "", nil); err == nil {
		t.Fatal("unknown client")
	}
	if _, err := m.Apply("opencode", "ftp://x", "k", "", nil); err == nil {
		t.Fatal("bad scheme")
	}
	if _, err := m.Restore("opencode"); err == nil {
		t.Fatal("nothing to restore")
	}
}
