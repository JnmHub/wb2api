package pool

import (
	"testing"
	"time"
)

func TestRemainingPriorityWeightsTheNearLimitAccounts(t *testing.T) {
	p := New(mkRecs(3, time.Hour))
	p.ApplyRemainingWeights(map[string]map[string]int{
		"m": {"u000000": 4, "u000001": 2},
	})
	counts := map[string]int{}
	for i := 0; i < 60; i++ {
		a := p.Pick("intl", "m", nil)
		if a == nil {
			t.Fatal("nil pick")
		}
		counts[a.UID]++
	}
	if counts["u000002"] != 0 {
		t.Fatalf("an unweighted account must not enter the priority layer: %v", counts)
	}
	if counts["u000000"] <= counts["u000001"] {
		t.Fatalf("the narrower remaining quota needs the bigger share: %v", counts)
	}
}

func TestRemainingPriorityFallsBackWhenTheOnlyWeightedAccountIsCooling(t *testing.T) {
	p := New(mkRecs(2, time.Hour))
	p.ApplyRemainingWeights(map[string]map[string]int{"m": {"u000000": 4}})
	acc := p.Get("u000000")
	acc.CoolDown(time.Now().Add(time.Minute))
	seen := map[string]int{}
	for i := 0; i < 10; i++ {
		a := p.Pick("intl", "m", nil)
		if a == nil {
			t.Fatal("the rotation must still serve when the preferred account is cooling")
		}
		seen[a.UID]++
	}
	if seen["u000000"] != 0 || seen["u000001"] != 10 {
		t.Fatalf("%v", seen)
	}
}

func TestRemainingWeightsArePerModel(t *testing.T) {
	p := New(mkRecs(2, time.Hour))
	p.ApplyRemainingWeights(map[string]map[string]int{"only-model": {"u000000": 4}})
	for i := 0; i < 10; i++ {
		if a := p.Pick("intl", "other-model", nil); a == nil {
			t.Fatal("nil")
		}
	}
	p.ApplyRemainingWeights(nil)
	if p.remaining("only-model") != nil {
		t.Fatal("clearing must remove the preference")
	}
}
