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
