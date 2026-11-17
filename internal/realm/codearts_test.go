package realm

import "testing"

func TestCodeArtsIsItsOwnFamily(t *testing.T) {
	if Family(CodeArts) != "codearts" {
		t.Fatalf("Family(%q) = %q, want codearts", CodeArts, Family(CodeArts))
	}
	for _, r := range []string{Intl, CN, "", "huawei", "unknown"} {
		if Family(r) != "workbuddy" {
			t.Fatalf("Family(%q) = %q, want workbuddy", r, Family(r))
		}
	}
	if SameFamily(CodeArts, CN) || SameFamily(CodeArts, Intl) {
		t.Fatal("CodeArts must not share a family with a WorkBuddy exit")
	}
	if !SameFamily(CodeArts, CodeArts) || !SameFamily(Intl, CN) {
		t.Fatal("SameFamily is inconsistent")
	}
}

func TestValidAcceptsOnlyKnownChannels(t *testing.T) {
	for _, r := range []string{Intl, CN, CodeArts, " CN ", "CodeArts"} {
		if !Valid(r) {
			t.Fatalf("Valid(%q) = false", r)
		}
	}
	for _, r := range []string{"", "huawei", "codeart", "intl2", "all"} {
		if Valid(r) {
			t.Fatalf("Valid(%q) = true, want false", r)
		}
	}
}

func TestIsCodeArts(t *testing.T) {
	if !IsCodeArts(CodeArts) || IsCodeArts(CN) || IsCodeArts("") || IsCodeArts(Intl) {
		t.Fatal("IsCodeArts is wrong")
	}
}

// A new channel must not inherit the WorkBuddy defaults: Get used to fall back
// to the international exit for anything unknown, which pointed the request at
// www.workbuddy.ai instead of the vendor's own host.
func TestGetCodeArtsConfig(t *testing.T) {
	cfg := Get(CodeArts)
	if cfg.ChatUpstream != CodeArtsChatBase {
		t.Fatalf("chat upstream = %q, want %q", cfg.ChatUpstream, CodeArtsChatBase)
	}
	if cfg.Domain != "" {
		t.Fatalf("domain = %q, want empty (the channel has no X-Domain header)", cfg.Domain)
	}
	if cfg.Name == "" || cfg.InfoFilename == "" || cfg.CacheDirName == "" {
		t.Fatalf("config is incomplete: %+v", cfg)
	}
	// The channel has a check-in (points), but it is a different operation from
	// the WorkBuddy chore; HasCheckin only says "there is something to claim".
	if !cfg.HasCheckin {
		t.Fatal("CodeArts has a daily points check-in")
	}
	if Get(CN).ChatUpstream == cfg.ChatUpstream || Get(Intl).ChatUpstream == cfg.ChatUpstream {
		t.Fatal("CodeArts must not share the WorkBuddy host")
	}
}

// The two vendors do not share a capacity model: the channel ceiling narrows
// the global per-account limit instead of replacing it.
func TestInflightForNarrowsPerChannel(t *testing.T) {
	t.Setenv("WB_CODEARTS_INFLIGHT", "")
	if got := InflightFor(CodeArts, 8); got != DefaultCodeArtsInflight {
		t.Fatalf("codearts with global 8 = %d, want %d", got, DefaultCodeArtsInflight)
	}
	if got := InflightFor(CodeArts, 1); got != 1 {
		t.Fatalf("codearts with global 1 = %d, want 1 (global is stricter)", got)
	}
	if got := InflightFor(CodeArts, 0); got != DefaultCodeArtsInflight {
		t.Fatalf("codearts with global 0 = %d, want the channel ceiling", got)
	}
	if got := InflightFor(Intl, 8); got != 8 {
		t.Fatalf("intl = %d, want the global value", got)
	}
	if got := InflightFor(CN, 0); got != 0 {
		t.Fatalf("cn = %d, want unlimited", got)
	}

	t.Setenv("WB_CODEARTS_INFLIGHT", "2")
	if got := InflightFor(CodeArts, 8); got != 2 {
		t.Fatalf("override = %d, want 2", got)
	}
	t.Setenv("WB_CODEARTS_INFLIGHT", "0")
	if got := InflightFor(CodeArts, 8); got != 8 {
		t.Fatalf("disabled ceiling = %d, want the global value", got)
	}
	t.Setenv("WB_CODEARTS_INFLIGHT", "not-a-number")
	if got := InflightFor(CodeArts, 8); got != DefaultCodeArtsInflight {
		t.Fatalf("garbage override = %d, want the default", got)
	}
}
