package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func readJSON(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

func TestPBKDF2Golden(t *testing.T) {
	var cases []struct {
		Password string `json:"password"`
		Salt     string `json:"salt"`
		Rounds   int    `json:"rounds"`
		Digest   string `json:"digest"`
	}
	readJSON(t, "pbkdf2.json", &cases)
	for _, c := range cases {
		got, err := digest(c.Password, c.Salt, c.Rounds)
		if err != nil || got != c.Digest {
			t.Errorf("digest(%q,%s,%d)=%s,%v want %s", c.Password, c.Salt, c.Rounds, got, err, c.Digest)
		}
	}
}

func TestPasswordLifecycle(t *testing.T) {
	s := Open(t.TempDir())
	if !s.PanelPasswordIsDefault() || !s.VerifyPanelPassword("admin") || s.VerifyPanelPassword("x") {
		t.Fatal("fresh install must accept only the default password")
	}
	if err := s.SetPanelPassword("s3cret-中文"); err != nil {
		t.Fatal(err)
	}
	if s.PanelPasswordIsDefault() || !s.VerifyPanelPassword("s3cret-中文") || s.VerifyPanelPassword("admin") {
		t.Fatal("custom password not enforced")
	}
	d := s.Get()
	if d["panel_password_hash"] == "" || d["panel_password_salt"] == "" {
		t.Fatal("digest not persisted")
	}
	raw, _ := os.ReadFile(s.Path())
	if string(raw) == "" || json.Valid(raw) == false {
		t.Fatal("settings.json invalid")
	}
	if err := s.SetPanelPassword("admin"); err != nil {
		t.Fatal(err)
	}
	if !s.PanelPasswordIsDefault() || !s.VerifyPanelPassword("admin") {
		t.Fatal("reset to default failed")
	}
}

// A password written by the legacy implementation must verify in Go.
func TestVerifyLegacyDigest(t *testing.T) {
	var cases []struct {
		Password, Salt string
		Rounds         int
		Digest         string
	}
	readJSON(t, "pbkdf2.json", &cases)
	dir := t.TempDir()
	c := cases[1]
	doc := map[string]any{
		"panel_password_hash": c.Digest, "panel_password_salt": c.Salt,
		"panel_password_rounds": c.Rounds, "panel_password_default": false,
	}
	b, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	s := Open(dir)
	if !s.VerifyPanelPassword(c.Password) || s.VerifyPanelPassword(c.Password+"x") {
		t.Fatal("legacy digest not verified")
	}
}

func TestUnknownKeysSurviveUpdate(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"),
		[]byte(`{"future":{"a":[1,2,{"b":true}]},"limits":{"global":{"x":1}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Open(dir)
	if err := s.Update(func(d Doc) error { d["new"] = "v"; return nil }); err != nil {
		t.Fatal(err)
	}
	d := s.Get()
	if d["new"] != "v" || d["future"] == nil || d["limits"] == nil {
		t.Fatalf("lost keys: %v", d)
	}
}

func TestConcurrentUpdatesDoNotLoseWrites(t *testing.T) {
	s := Open(t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := string(rune('a' + i))
			if err := s.Update(func(d Doc) error { d[key] = true; return nil }); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if n := len(s.Get()); n != 20 {
		t.Fatalf("lost writes: %d of 20 keys", n)
	}
}

func TestCacheSeesExternalEdit(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	_ = s.Get()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(CheckInterval + 100*time.Millisecond) // reads are throttled to one stat per interval
	if _, ok := s.Get()["a"]; !ok {
		t.Fatal("external creation not noticed")
	}
}

func TestDeepMerge(t *testing.T) {
	base := map[string]any{"a": map[string]any{"x": 1, "y": 2}, "k": "v"}
	patch := map[string]any{"a": map[string]any{"y": 3, "z": 4}}
	got := DeepMerge(base, patch)
	want := map[string]any{"a": map[string]any{"x": 1, "y": 3, "z": 4}, "k": "v"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if base["a"].(map[string]any)["y"] != 2 {
		t.Fatal("base mutated")
	}
}

func TestFnMatchGolden(t *testing.T) {
	var cases []struct {
		Pattern, Name string
		Out           bool
	}
	readJSON(t, "fnmatch.json", &cases)
	for _, c := range cases {
		if got := FnMatchCase(c.Name, c.Pattern); got != c.Out {
			t.Errorf("FnMatchCase(%q,%q)=%v want %v", c.Name, c.Pattern, got, c.Out)
		}
	}
}

func apiKeysStore(t *testing.T) *Store {
	t.Helper()
	var docs []map[string]any
	readJSON(t, "api_keys_doc.json", &docs)
	dir := t.TempDir()
	b, _ := json.Marshal(docs[0])
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return Open(dir)
}

func TestAPIKeysGolden(t *testing.T) {
	var want []map[string]any
	readJSON(t, "api_keys_expected.json", &want)
	got := apiKeysStore(t).APIKeys(true)
	if len(got) != len(want) {
		t.Fatalf("got %d keys, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		cmp := func(name string, a, b any) {
			if !reflect.DeepEqual(a, b) {
				t.Errorf("key %d %s: got %#v want %#v", i, name, a, b)
			}
		}
		cmp("id", g.ID, w["id"])
		cmp("name", g.Name, w["name"])
		cmp("key", g.Key, w["key"])
		cmp("realm", g.Realm, w["realm"])
		cmp("enabled", g.Enabled, w["enabled"])
		cmp("deleted_at", g.DeletedAt, w["deleted_at"])
		if g.ID == "k1" {
			// legacy stamps "now" when missing; Go keeps it empty to stay deterministic
			cmp("created_at", g.CreatedAt, w["created_at"])
		}
		if c, ok := w["concurrency_limit"].(float64); ok {
			cmp("concurrency", g.ConcurrencyLimit, int(c))
		}
		var models []string
		for _, m := range w["models"].([]any) {
			models = append(models, m.(string))
		}
		if len(models)+len(g.Models) > 0 {
			cmp("models", g.Models, models)
		}
	}
	if n := len(apiKeysStore(t).APIKeys(false)); n != len(want)-1 {
		t.Errorf("deleted entry should be hidden by default, got %d", n)
	}
}

func TestKeyAllowsGolden(t *testing.T) {
	var cases []struct {
		KeyID string `json:"key_id"`
		Model string `json:"model"`
		Out   bool   `json:"out"`
	}
	readJSON(t, "key_allows.json", &cases)
	byID := map[string]APIKey{}
	for _, k := range apiKeysStore(t).APIKeys(true) {
		byID[k.ID] = k
	}
	for _, c := range cases {
		if got := byID[c.KeyID].AllowsModel(c.Model); got != c.Out {
			t.Errorf("key %s AllowsModel(%q)=%v want %v", c.KeyID, c.Model, got, c.Out)
		}
	}
}

func TestLegacySingleKey(t *testing.T) {
	var want []map[string]any
	readJSON(t, "api_keys_legacy.json", &want)
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"api_key_set": true, "api_key": " legacy-k "}`), 0o644)
	got := Open(dir).APIKeys(false)
	if len(got) != 1 || got[0].ID != want[0]["id"] || got[0].Key != want[0]["key"] || got[0].Name != want[0]["name"] {
		t.Fatalf("got %+v want %v", got, want)
	}
}
