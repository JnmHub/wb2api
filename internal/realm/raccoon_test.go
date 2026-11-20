package realm

import "testing"

// TestRaccoonChannel locks the fourth channel's contract: it is its own family,
// it is a valid persisted realm, and it does not inherit any WorkBuddy
// behaviour.
func TestRaccoonChannel(t *testing.T) {
	if !Valid(Raccoon) {
		t.Fatalf("Valid(%q) = false", Raccoon)
	}
	if Family(Raccoon) != "raccoon" {
		t.Fatalf("Family(%q) = %q, want raccoon", Raccoon, Family(Raccoon))
	}
	if !IsRaccoon(Raccoon) || IsRaccoon(CodeArts) || IsRaccoon(CN) || IsRaccoon(Intl) {
		t.Fatal("IsRaccoon must match exactly one channel")
	}
	if SameFamily(Raccoon, CN) || SameFamily(Raccoon, CodeArts) {
		t.Fatal("raccoon must not share a family with another channel")
	}
	cfg := Get(Raccoon)
	if cfg.ChatUpstream != RaccoonChatBase || cfg.Domain != "" || !cfg.HasCheckin {
		t.Fatalf("unexpected raccoon config: %+v", cfg)
	}
	// Existing channels keep their verdicts.
	for _, r := range []string{Intl, CN} {
		if Family(r) != "workbuddy" || !Valid(r) {
			t.Fatalf("workbuddy channel %q changed: family=%q valid=%v", r, Family(r), Valid(r))
		}
	}
	if Family(CodeArts) != "codearts" || !Valid(CodeArts) {
		t.Fatal("codearts channel changed")
	}
	if Valid("nope") {
		t.Fatal("unknown realm must stay invalid")
	}
}

// TestInflightForChannels verifies the ceiling is narrowed per channel and the
// WorkBuddy channels keep the global value untouched.
func TestInflightForChannels(t *testing.T) {
	t.Setenv("WB_RACCOON_INFLIGHT", "2")
	t.Setenv("WB_CODEARTS_INFLIGHT", "3")
	cases := []struct {
		realm  string
		global int
		want   int
	}{
		{CN, 8, 8},
		{Intl, 8, 8},
		{CodeArts, 8, 3},
		{Raccoon, 8, 2},
		{Raccoon, 1, 1},
		{Raccoon, 0, 2},
	}
	for _, c := range cases {
		if got := InflightFor(c.realm, c.global); got != c.want {
			t.Fatalf("InflightFor(%q, %d) = %d, want %d", c.realm, c.global, got, c.want)
		}
	}
	// 0 disables the channel ceiling.
	t.Setenv("WB_RACCOON_INFLIGHT", "0")
	if got := InflightFor(Raccoon, 8); got != 8 {
		t.Fatalf("InflightFor with ceiling 0 = %d, want 8", got)
	}
}
