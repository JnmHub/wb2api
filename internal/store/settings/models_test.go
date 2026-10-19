package settings

import (
	"strings"
	"testing"
)

func TestModelBanListPrecedence(t *testing.T) {
	s := Open(t.TempDir())
	if s.ModelBanned("gpt-5.5") {
		t.Fatal("a fresh install bans nothing")
	}
	if _, err := s.SetBannedModels([]string{"GPT-5.5", "hy3", "GPT-5.5"}); err != nil {
		t.Fatal(err)
	}
	if !s.ModelBanned("gpt-5.5") || !s.ModelBanned("HY3") {
		t.Fatal("the exact list must match case-insensitively")
	}
	if s.ModelBanned("gpt-5.5-extra") {
		t.Fatal("the exact list must not act as a prefix")
	}
	if err := s.SetBannedSubstring("Image-Alpha"); err != nil {
		t.Fatal(err)
	}
	if !s.ModelBanned("flux-image-alpha-v2") {
		t.Fatal("the substring rule must match case-insensitively")
	}
	// The allowlist wins over both deny rules.
	if err := s.Update(func(d Doc) error {
		d["allowed_models"] = stringsToAny([]string{"GPT-5.5"})
		d["allowed_prefixes"] = stringsToAny([]string{"HY"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if s.ModelBanned("gpt-5.5") {
		t.Fatal("an explicit allow entry must win")
	}
	if s.ModelBanned("hy3") {
		t.Fatal("an allow prefix must win")
	}
	if !s.ModelBanned("flux-image-alpha-v2") {
		t.Fatal("the allowlist must not lift unrelated denies")
	}
	if got := s.BannedModels(); len(got) != 2 || got[0] != "gpt-5.5" {
		t.Fatalf("stored list %v", got)
	}
	if strings.Join(s.BannedModels(), ",") != "gpt-5.5,hy3" {
		t.Fatalf("stored list %v", s.BannedModels())
	}
}
