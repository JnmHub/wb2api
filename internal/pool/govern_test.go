package pool

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"wb2api/internal/store/accounts"
)

func newAcct(t *testing.T, realm string) *Account {
	t.Helper()
	rec := accounts.FromMap(map[string]any{"uid": "u1", "accessToken": "tok", "realm": realm,
		"expiresAt": time.Now().Add(24 * time.Hour).Unix()}, "", time.Now())
	return NewAccount(rec)
}

func TestBackoffLaddersMatchLegacy(t *testing.T) {
	raw, err := os.ReadFile("../gateway/ratelimit/testdata/ratelimit.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Backoff map[string][]float64 `json:"backoff"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	fns := map[string]func(int) time.Duration{"soft": SoftBackoff, "unverified": UnverifiedSoftBackoff,
		"breaker": BreakerBackoff, "degrade": DegradeBackoff}
	for name, want := range v.Backoff {
		for n, w := range want {
			if got := fns[name](n).Seconds(); got != w {
				t.Errorf("%s(%d) = %v want %v", name, n, got, w)
			}
		}
	}
}

func TestSoftRateLadderAndDecay(t *testing.T) {
	a := newAcct(t, "intl")
	now := time.Now()
	for i, want := range []time.Duration{600 * time.Second, 1200 * time.Second, 2400 * time.Second} {
		if got := a.NoteSoftRate("429", now); got != want {
			t.Errorf("streak %d: %v want %v", i+1, got, want)
		}
	}
	if a.Ready("m", now) {
		t.Error("soft-limited account must not be ready")
	}
	// a long gap makes the next hit start again at the first tier
	if got := a.NoteSoftRate("429", now.Add(FailWindow+time.Second)); got != 600*time.Second {
		t.Errorf("after the window the ladder restarts: %v", got)
	}
}

func TestUnscopedLadderIsSeparate(t *testing.T) {
	a := newAcct(t, "intl")
	now := time.Now()
	for i := 0; i < 4; i++ {
		a.NoteUnscopedRate(now)
	}
	if got := a.NoteSoftRate("scoped", now); got != 600*time.Second {
		t.Errorf("unscoped hits must not push the credential ladder: %v", got)
	}
}

func TestModelErrorThrottlesOnlyThatModel(t *testing.T) {
	a := newAcct(t, "intl")
	now := time.Now()
	a.NoteModelError("m1", time.Time{}, 60*time.Second, "429", "body", now)
	if a.Ready("m1", now) || !a.Ready("m2", now) {
		t.Error("only the throttled model is refused")
	}
	if w := a.ThrottleWait("m1", now); w < 59*time.Second || w > 61*time.Second {
		t.Errorf("wait %v", w)
	}
	if msg := a.UnavailableReason("m1", now); msg == "" {
		t.Error("reason must explain the refusal")
	}
	a.NoteModelError("m3", now.Add(2*time.Hour), 0, "429", "", now)
	if w := a.ThrottleWait("m3", now); w < 119*time.Minute {
		t.Errorf("a reset clock wins over the default wait: %v", w)
	}
}

func TestBreakerTripsOnThirdFailureAndSuccessClears(t *testing.T) {
	a := newAcct(t, "intl")
	now := time.Now()
	a.NoteFailure("502", now)
	a.NoteFailure("502", now)
	if !a.Ready("m", now) {
		t.Fatal("two failures do not trip the breaker")
	}
	a.NoteFailure("502", now)
	if a.Ready("m", now) || a.ThrottleWait("m", now) < 29*time.Minute {
		t.Fatalf("breaker should hold ~30 min: %v", a.ThrottleWait("m", now))
	}
	a.NoteSuccess("m")
	if !a.Ready("m", now) {
		t.Error("a served request clears the breaker")
	}
}

func TestFailuresFarApartAreNotConsecutive(t *testing.T) {
	a := newAcct(t, "intl")
	now := time.Now()
	a.NoteFailure("x", now)
	a.NoteFailure("x", now.Add(FailWindow+time.Second))
	a.NoteFailure("x", now.Add(2*FailWindow+2*time.Second))
	if !a.Ready("m", now.Add(2*FailWindow+3*time.Second)) {
		t.Error("three failures spread over hours must not trip the breaker")
	}
}

func TestUnknownFailuresDegradeAfterFive(t *testing.T) {
	a := newAcct(t, "intl")
	now := time.Now()
	for i := 0; i < 4; i++ {
		a.NoteUnknownFailure("?", now)
	}
	// 3rd already tripped the breaker via the shared counter; check degrade state separately
	_, _, fails, degrade := a.Streaks()
	if fails != 4 || degrade != 4 {
		t.Fatalf("counters %d %d", fails, degrade)
	}
	a.NoteUnknownFailure("?", now)
	if a.degradeTo.Load() == 0 {
		t.Error("fifth unknown failure opens the degrade window")
	}
}

func TestBalanceParkUntilFourAM(t *testing.T) {
	a := newAcct(t, "cn")
	now := time.Date(2026, 10, 19, 15, 0, 0, 0, time.Local)
	until := a.NoteBalanceCooled("402", now)
	if until.Hour() != 4 || until.Day() != 20 {
		t.Errorf("until %v", until)
	}
	if early := NextLocal4AM(time.Date(2026, 10, 19, 3, 0, 0, 0, time.Local)); early.Day() != 19 {
		t.Errorf("before 4am the same day: %v", early)
	}
	if a.Ready("m", now) {
		t.Error("parked")
	}
	next := *a.Record()
	next.Credits = map[string]any{"remain": json.Number("5")}
	a.rec.Store(&next)
	if !a.ReviveBalanceCooldown() || !a.Ready("m", now) {
		t.Error("a refresh showing credits lifts the park")
	}
}

// "Clear cooling" is the operator's "make it selectable again" button: it has
// to lift the balance park the 429 exhausted-quota verdict writes, otherwise the
// account would look unclearable until 04:00.
func TestClearCooldownsLiftsTheBalancePark(t *testing.T) {
	a := newAcct(t, "cn")
	now := time.Date(2026, 10, 29, 19, 0, 0, 0, time.Local)
	a.NoteBalanceCooled("HTTP 429 quota", now)
	if a.Ready("m", now) {
		t.Fatal("parked account must not be ready")
	}
	if st := a.State(now); st != StatusCooling {
		t.Fatalf("state = %s want cooling", st)
	}
	a.ClearCooldowns(false)
	if !a.Ready("m", now) {
		t.Fatal("clear cooling must lift the balance park")
	}
	if st := a.State(now); st != StatusReady {
		t.Fatalf("state after clearing = %s want ready", st)
	}
}

func TestModelCapOnlyForFreeModelsAndPersisted(t *testing.T) {
	a := newAcct(t, "intl")
	now := time.Now()
	reset := now.Add(3 * time.Hour)
	rec, ok := a.NoteModelCap("hy3", reset, now)
	if !ok || rec == nil || rec.ModelCaps["hy3"].Reset == 0 {
		t.Fatalf("free model cap must be recorded: %v %v", rec, ok)
	}
	if _, again := a.NoteModelCap("hy3", reset, now); again {
		t.Error("same window again is not new")
	}
	if a.Ready("hy3", now) {
		t.Error("capped model must be cooling")
	}
	// clearing keeps the cap
	a.ClearError("", true, now)
	if a.Ready("hy3", now) {
		t.Error("keepCaps keeps the window")
	}
	// restart: a new Account from the persisted record re-arms the cooldown
	b := NewAccount(rec)
	if b.Ready("hy3", now) || len(b.CappedModels(now)) != 1 {
		t.Error("caps survive a restart")
	}
}

func TestSuccessLiftsRestrictionAndReturnsRecord(t *testing.T) {
	a := newAcct(t, "intl")
	now := time.Now()
	a.NoteRestricted(now, RestrictedAccountCooldown)
	if a.Ready("m", now) {
		t.Fatal("restricted")
	}
	if rec := a.NoteSuccess("m"); rec == nil || rec.RestrictedUntil != 0 {
		t.Error("success lifts the persisted restriction")
	}
}

func TestGuardsRefuseWork(t *testing.T) {
	a := newAcct(t, "intl")
	now := time.Now()
	a.SetGuards(GuardState{CreditReached: true, Free: map[string]bool{"hy3": true}})
	if !a.Ready("hy3", now) || a.Ready("gpt-5.5", now) {
		t.Error("credit cap stops paid models only")
	}
	a.SetGuards(GuardState{DailyToken: true})
	if a.Ready("hy3", now) {
		t.Error("daily token budget parks the account")
	}
	a.SetGuards(GuardState{ModelBlocked: map[string]bool{"m1": true}})
	if a.Ready("m1", now) || !a.Ready("m2", now) {
		t.Error("model budget stops that model only")
	}
}
