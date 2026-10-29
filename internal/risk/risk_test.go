package risk

import (
	"testing"
	"time"
)

func TestIsAccountRestriction(t *testing.T) {
	yes := []string{
		`{"code":11140,"msg":"request illegal"}`,
		`{"code": "11140"}`,
		`{"displayMsg":{"en":"Your account has been restricted due to unusual activity"}}`,
		`账号存在异常行为，已被限制使用`,
		`UNUSUAL ACTIVITY`,
	}
	no := []string{"", `{"code":11128}`, `{"requestId":"a11140b"}`, `content rejected`}
	for _, s := range yes {
		if !IsAccountRestriction(s) {
			t.Errorf("should be restriction: %q", s)
		}
	}
	for _, s := range no {
		if IsAccountRestriction(s) {
			t.Errorf("should not be restriction: %q", s)
		}
	}
}

func TestWavePausesRealmOnlyForDistinctAccounts(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	g := &Guard{Cfg: Config{WaveMin: 2, WaveWindow: 15 * time.Minute, Hold: 30 * time.Minute}, Now: func() time.Time { return now }}
	if !g.Note("intl", "a").IsZero() {
		t.Fatal("one restricted account must not pause the realm")
	}
	if !g.Note("intl", "a").IsZero() {
		t.Fatal("the same account again must not look like a wave")
	}
	if g.HoldRemaining("intl") != 0 {
		t.Fatal("no pause expected yet")
	}
	until := g.Note("intl", "b")
	if until.IsZero() || g.HoldRemaining("intl") != 30*time.Minute {
		t.Fatalf("two distinct accounts must pause for 30 min, until=%v rem=%v", until, g.HoldRemaining("intl"))
	}
	if g.HoldRemaining("cn") != 0 {
		t.Fatal("other realms keep running")
	}
	now = now.Add(31 * time.Minute)
	if g.HoldRemaining("intl") != 0 {
		t.Fatal("pause must expire")
	}
	g.Reset()
	if g.HoldRemaining("intl") != 0 {
		t.Fatal("reset clears")
	}
}

func TestWaveWindowForgetsOldEvents(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	g := &Guard{Cfg: Config{WaveMin: 2, WaveWindow: 15 * time.Minute, Hold: time.Hour}, Now: func() time.Time { return now }}
	g.Note("intl", "a")
	now = now.Add(20 * time.Minute)
	if !g.Note("intl", "b").IsZero() {
		t.Fatal("an event outside the window must not count")
	}
}

func TestHoldZeroDisablesPause(t *testing.T) {
	g := &Guard{Cfg: Config{WaveMin: 1, WaveWindow: time.Hour, Hold: 0}}
	if !g.Note("intl", "a").IsZero() {
		t.Fatal("Hold=0 turns the pause off")
	}
}

// Defaults must match the "tag the account, never lock the gateway" policy:
// a restricted account is parked by the gateway and skipped, but dispatch to
// its realm keeps running, and the request may still rotate to another account.
func TestDefaultPolicyParksTheAccountNotTheGateway(t *testing.T) {
	for _, k := range []string{"WB_RESTRICTION_HOLD_SECONDS", "WB_RESTRICTED_403_ROTATE_MAX", "WB_RESTRICTION_WAVE_MIN", "WB_RESTRICTION_WAVE_WINDOW"} {
		t.Setenv(k, "")
	}
	cfg := ConfigFromEnv()
	if cfg.Hold != 0 {
		t.Fatalf("realm pause must be opt-in, got Hold=%v", cfg.Hold)
	}
	if cfg.RotateMax != 2 {
		t.Fatalf("one restriction should still be able to rotate to another account, got RotateMax=%d", cfg.RotateMax)
	}
	now := time.Unix(1_800_000_000, 0)
	g := &Guard{Cfg: cfg, Now: func() time.Time { return now }}
	for _, uid := range []string{"a", "b", "c", "d"} {
		if until := g.Note("intl", uid); !until.IsZero() {
			t.Fatalf("default policy must never pause the realm, paused until %v", until)
		}
	}
	if rem := g.HoldRemaining("intl"); rem != 0 {
		t.Fatalf("HoldRemaining = %v, want 0", rem)
	}
}

func TestOperatorsCanTurnTheRealmPauseBackOn(t *testing.T) {
	t.Setenv("WB_RESTRICTION_HOLD_SECONDS", "600")
	t.Setenv("WB_RESTRICTION_WAVE_MIN", "2")
	cfg := ConfigFromEnv()
	if cfg.Hold != 10*time.Minute {
		t.Fatalf("Hold = %v, want 10m", cfg.Hold)
	}
	now := time.Unix(1_800_000_000, 0)
	g := &Guard{Cfg: cfg, Now: func() time.Time { return now }}
	g.Note("intl", "a")
	if until := g.Note("intl", "b"); until.IsZero() {
		t.Fatal("opt-in pause must trigger on a wave")
	}
	if rem := g.HoldRemaining("intl"); rem != 10*time.Minute {
		t.Fatalf("HoldRemaining = %v, want 10m", rem)
	}
}
