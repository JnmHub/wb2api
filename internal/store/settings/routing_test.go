package settings

import "testing"

func TestModelAliasesRoundTripAndResolve(t *testing.T) {
	s := Open(t.TempDir())
	if _, ok := s.ResolveModelAlias("claude-sonnet-4"); ok {
		t.Fatal("expected no alias before any are configured")
	}
	if err := s.SetModelAliases(map[string]string{
		"Claude-Sonnet-4": "glm-4.7",
		"same":            "same", // self-mapping is dropped
		"":                "x",    // empty name dropped
		"bad":             " ",    // empty target dropped
	}); err != nil {
		t.Fatalf("SetModelAliases: %v", err)
	}
	got, ok := s.ResolveModelAlias("CLAUDE-SONNET-4")
	if !ok || got != "glm-4.7" {
		t.Fatalf("ResolveModelAlias = %q, %v; want glm-4.7, true", got, ok)
	}
	if aliases := s.ModelAliases(); len(aliases) != 1 {
		t.Fatalf("aliases = %v; want only the real mapping", aliases)
	}
	if err := s.SetModelAliases(nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if s.ModelAliases() != nil {
		t.Fatal("clearing the map must remove the key")
	}
}

func TestModelsOnlyAvailableToggle(t *testing.T) {
	s := Open(t.TempDir())
	if s.ModelsOnlyAvailable() {
		t.Fatal("availability filter must default to off")
	}
	if err := s.SetModelsOnlyAvailable(true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !s.ModelsOnlyAvailable() {
		t.Fatal("expected the filter on after enabling it")
	}
	if err := s.SetModelsOnlyAvailable(false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if s.ModelsOnlyAvailable() {
		t.Fatal("expected the filter off after disabling it")
	}
	if _, ok := s.View()["models_only_available"]; ok {
		t.Fatal("disabling must delete the key instead of storing false")
	}
}

func TestKeyAccountPatterns(t *testing.T) {
	k := APIKey{}
	if !k.AllowsAccount("u000012") || k.Restricted() {
		t.Fatal("a key without patterns must allow every account")
	}
	k = APIKey{Accounts: []string{"u000012", "u0000*"}}
	if !k.AllowsAccount("U000099") {
		t.Fatal("wildcard pattern must match case-insensitively")
	}
	if k.AllowsAccount("u111111") {
		t.Fatal("account outside the patterns must be rejected")
	}
	if !k.Restricted() {
		t.Fatal("non-empty patterns must mark the key restricted")
	}
	patterns := cleanAccountPatterns([]any{"U000012", "u000012", " u000013 ", ""})
	if len(patterns) != 2 || patterns[0] != "u000012" || patterns[1] != "u000013" {
		t.Fatalf("cleanAccountPatterns = %v; want lower-cased de-duplicated list", patterns)
	}
}
