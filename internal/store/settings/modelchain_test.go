package settings

import (
	"reflect"
	"testing"
)

func TestModelChainKeepsOrderAndDropsSelf(t *testing.T) {
	s := Open(t.TempDir())
	if err := s.SetModelFallbacks(map[string][]string{
		"DeepSeek-V4.1-Flash": {"space-bunny", "deepseek-v4.1-flash", "space-bunny", " ", "gpt-5.2"},
	}); err != nil {
		t.Fatal(err)
	}
	got := s.ModelChain("deepseek-v4.1-flash")
	want := []string{"deepseek-v4.1-flash", "space-bunny", "gpt-5.2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chain = %v, want %v", got, want)
	}
	if chain := s.ModelChain("glm-5.3"); !reflect.DeepEqual(chain, []string{"glm-5.3"}) {
		t.Fatalf("unconfigured model chain = %v", chain)
	}
}

func TestModelChainIsCapped(t *testing.T) {
	s := Open(t.TempDir())
	var long []string
	for i := 0; i < MaxModelChain+5; i++ {
		long = append(long, "m"+string(rune('a'+i)))
	}
	if err := s.SetModelFallbacks(map[string][]string{"head": long}); err != nil {
		t.Fatal(err)
	}
	got := s.ModelFallbacks()["head"]
	if len(got) != MaxModelChain {
		t.Fatalf("stored %d fallbacks, want %d", len(got), MaxModelChain)
	}
	if len(s.ModelChain("head")) != MaxModelChain+1 {
		t.Fatalf("chain length = %d", len(s.ModelChain("head")))
	}
}

func TestModelFallbacksClearsWhenEmpty(t *testing.T) {
	s := Open(t.TempDir())
	if err := s.SetModelFallbacks(map[string][]string{"a": {"b"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetModelFallbacks(nil); err != nil {
		t.Fatal(err)
	}
	if fb := s.ModelFallbacks(); fb != nil {
		t.Fatalf("fallbacks survived a clear: %v", fb)
	}
	if chain := s.ModelChain("a"); len(chain) != 1 || chain[0] != "a" {
		t.Fatalf("cleared chain = %v", chain)
	}
}
