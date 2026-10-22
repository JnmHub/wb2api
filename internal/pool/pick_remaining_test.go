package pool

import (
	"fmt"
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

func TestRemainingSnapshotImmutableAndZeroAllocation(t *testing.T) {
	p := New(mkRecs(3, time.Hour))
	weights := []RemainingWeight{{Realm: "intl", Model: "m", UID: "u000000", Weight: 4}, {Realm: "intl", Model: "m", UID: "u000001", Weight: 2}}
	s := NewRemainingSnapshot(12, weights)
	weights[0].Weight = 99
	if s.Revision() != 12 || s.Weight("intl", "m", "u000000") != 4 || s.Weight("other", "m", "u000000") != 1 || s.Weight("intl", "other", "u000000") != 1 {
		t.Fatal("realm/model isolation or defensive copy broken")
	}
	p.ApplyRemainingSnapshot(s)
	// Warm preferences and all dispatch state before counting.
	for i := 0; i < 10; i++ {
		p.Pick("intl", "m", nil)
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		p.ApplyRemainingSnapshot(s)
		if p.Pick("intl", "m", nil) == nil {
			panic("nil")
		}
	}); allocs != 0 {
		t.Fatalf("allocs=%v", allocs)
	}
	p.ApplyRemainingSnapshot(nil)
	if p.remainSnapshot.Load() != nil {
		t.Fatal("disabled advisory retained")
	}
}

func TestRemainingSnapshotSkipsExcludedAndPagedAccounts(t *testing.T) {
	p := New(mkRecs(3, time.Hour))
	p.ApplyRemainingSnapshot(NewRemainingSnapshot(1, []RemainingWeight{{Realm: "intl", Model: "m", UID: "u000000", Weight: 4}, {Realm: "intl", Model: "m", UID: "u000001", Weight: 3}}))
	exclude := map[string]struct{}{"u000000": {}}
	if a := p.Pick("intl", "m", exclude); a == nil || a.UID != "u000001" {
		t.Fatalf("excluded preference: %v", a)
	}
	if a := p.pick("intl", "m", nil, &Page{Index: 1, Size: 2}); a == nil || a.UID != "u000001" {
		t.Fatalf("page preference: %v", a)
	}
}

func BenchmarkPickRemainingSnapshot(b *testing.B) {
	for _, n := range []int{100, 1000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			p := New(mkRecs(n, time.Hour))
			weights := make([]RemainingWeight, 0, n)
			for _, a := range p.All() {
				weights = append(weights, RemainingWeight{Realm: "intl", Model: "m", UID: a.UID, Weight: 4})
			}
			s := NewRemainingSnapshot(1, weights)
			p.ApplyRemainingSnapshot(s)
			p.Pick("intl", "m", nil)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				p.ApplyRemainingSnapshot(s)
				p.Pick("intl", "m", nil)
			}
		})
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
