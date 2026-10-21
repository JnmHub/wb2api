package pool

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"wb2api/internal/store/accounts"
)

func mkRecs(n int, expiresIn time.Duration) []*accounts.Record {
	recs := make([]*accounts.Record, n)
	exp := time.Now().Add(expiresIn).Unix()
	for i := range recs {
		recs[i] = &accounts.Record{
			UID: fmt.Sprintf("u%06d", i), Realm: "intl", AccessToken: "t",
			ExpiresAt: exp, Weight: 1,
		}
	}
	return recs
}

func TestPickRotates(t *testing.T) {
	p := New(mkRecs(5, time.Hour))
	seen := map[string]int{}
	for i := 0; i < 50; i++ {
		a := p.Pick("intl", "m", nil)
		if a == nil {
			t.Fatal("nil pick")
		}
		seen[a.UID]++
	}
	if len(seen) != 5 {
		t.Fatalf("rotation touched %d accounts: %v", len(seen), seen)
	}
	for uid, c := range seen {
		if c != 10 {
			t.Errorf("%s served %d, want 10", uid, c)
		}
	}
}

func TestReadinessRules(t *testing.T) {
	p := New(mkRecs(3, time.Hour))
	all := p.All()
	all[0].SetEnabled(false)
	all[1].CoolDownModel("gpt", time.Now().Add(time.Minute))
	for i := 0; i < 20; i++ {
		a := p.Pick("intl", "gpt", nil)
		if a == nil || a.UID != all[2].UID {
			t.Fatalf("only the third account is ready for gpt, got %v", a)
		}
	}
	if a := p.Pick("intl", "glm", nil); a == nil || a.UID == all[0].UID {
		t.Fatal("cooldown on one model must not block other models; disabled stays out")
	}
	all[2].CoolDown(time.Now().Add(time.Minute))
	if a := p.Pick("intl", "gpt", nil); a != nil {
		t.Fatalf("nobody should be ready, got %s", a.UID)
	}
}

func TestExcludeAndTokenBad(t *testing.T) {
	p := New(mkRecs(3, time.Hour))
	ex := map[string]struct{}{"u000000": {}, "u000001": {}}
	for i := 0; i < 10; i++ {
		if a := p.Pick("intl", "", ex); a == nil || a.UID != "u000002" {
			t.Fatalf("got %v", a)
		}
	}
	p.All()[2].MarkTokenBad(true)
	if a := p.Pick("intl", "", ex); a != nil {
		t.Fatal("token-bad account must not be picked")
	}
}

// The request path must never refresh a token: a credential inside its last
// two minutes is simply not ready until the background refresher renews it.
func TestExpiringTokenIsNotReadyAndNeverBlocks(t *testing.T) {
	p := New(mkRecs(2, 30*time.Second))
	done := make(chan struct{})
	go func() {
		if a := p.Pick("intl", "", nil); a != nil {
			t.Error("expiring accounts must not be picked")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Pick blocked")
	}
	p.All()[0].SetExpiresAt(time.Now().Add(time.Hour).Unix())
	if a := p.Pick("intl", "", nil); a == nil || a.UID != "u000000" {
		t.Fatalf("renewed account should be ready, got %v", a)
	}
}

func TestMaxInflight(t *testing.T) {
	p := New(mkRecs(1, time.Hour))
	a := p.All()[0]
	a.SetMaxInflight(2)
	a.Acquire()
	a.Acquire()
	if p.Pick("intl", "", nil) != nil {
		t.Fatal("account at its in-flight cap must not be picked")
	}
	a.Release()
	if p.Pick("intl", "", nil) == nil {
		t.Fatal("capacity freed but not picked")
	}
}

func TestScorerOverridesLoadTie(t *testing.T) {
	p := New(mkRecs(2, time.Hour))
	p.SetTopK(1)
	p.SetScorer(func(a *Account, _ string) int64 {
		if a.UID == "u000001" {
			return 5
		}
		return 500
	})
	for i := 0; i < 12; i++ {
		a := p.Pick("intl", "", nil)
		if a == nil || a.UID != "u000001" {
			t.Fatalf("feedback rank should win the tie, got %v", a)
		}
	}
}

func TestMaxInflightGetter(t *testing.T) {
	a := NewAccount(mkRecs(1, time.Hour)[0])
	a.SetMaxInflight(7)
	if got := a.MaxInflight(); got != 7 {
		t.Fatalf("MaxInflight = %d, want 7", got)
	}
}

func TestConcurrentPickAndMutate(t *testing.T) {
	p := New(mkRecs(200, time.Hour))
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				if a := p.Pick("intl", "m", nil); a != nil {
					a.Acquire()
					if i%97 == 0 {
						a.CoolDownModel("m", time.Now().Add(time.Millisecond))
					}
					a.Release()
				}
				if g == 0 && i%500 == 0 {
					p.Replace(p.All()) // copy-on-write swap under load
				}
			}
		}(g)
	}
	wg.Wait()
	for _, a := range p.All() {
		if a.Inflight() != 0 {
			t.Fatalf("%s leaked in-flight count %d", a.UID, a.Inflight())
		}
	}
}

func benchPick(b *testing.B, n int, coolingFrac float64) {
	p := New(mkRecs(n, time.Hour))
	until := time.Now().Add(time.Hour)
	for i, a := range p.All() {
		if float64(i) < coolingFrac*float64(n) {
			a.CoolDownModel("m", until)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if p.Pick("intl", "m", nil) == nil {
				b.Error("nil")
				return
			}
		}
	})
}

func BenchmarkPick100(b *testing.B)        { benchPick(b, 100, 0) }
func BenchmarkPick1000(b *testing.B)       { benchPick(b, 1000, 0) }
func BenchmarkPick5000(b *testing.B)       { benchPick(b, 5000, 0) }
func BenchmarkPick5000Cool90(b *testing.B) { benchPick(b, 5000, 0.9) }

func TestZeroCreditIsNotSchedulable(t *testing.T) {
	now := time.Now()
	recs := mkRecs(2, time.Hour)
	recs[0].Credits = map[string]any{"remain": 0}
	recs[1].Credits = map[string]any{"remain": 3.5}
	p := New(recs)
	if got := p.All()[0].State(now); got != StatusNoCredit {
		t.Fatalf("zero-credit state = %s", got)
	}
	for i := 0; i < 20; i++ {
		a := p.Pick("intl", "m", nil)
		if a == nil || a.UID != recs[1].UID {
			t.Fatalf("picked %v, want only the funded account", a)
		}
	}
	solo := New([]*accounts.Record{recs[0]})
	if a := solo.Pick("intl", "m", nil); a != nil {
		t.Fatalf("an exhausted account must not be picked: %v", a.UID)
	}
	// Unknown balances are still eligible (the panel cannot know them yet).
	recs[0].Credits = nil
	if a := New([]*accounts.Record{recs[0]}).Pick("intl", "m", nil); a == nil {
		t.Fatal("an account with unknown credits should remain eligible")
	}
}

func TestCreditExpiringWithin(t *testing.T) {
	rec := mkRecs(1, time.Hour)[0]
	rec.Credits = map[string]any{"remain": 10, "packages": []any{
		map[string]any{"days_left": 3, "remain": 4},
		map[string]any{"days_left": 30, "remain": 6},
		map[string]any{"days_left": 1, "remain": 2, "no_expiry": true},
		map[string]any{"days_left": 2, "remain": 5, "is_expired": true},
	}}
	a := New([]*accounts.Record{rec}).All()[0]
	if got := a.CreditExpiringWithin(7); got != 4 {
		t.Fatalf("expiring within 7d = %v, want 4", got)
	}
}

func TestStateShowsBreakerAndBalanceAsCooling(t *testing.T) {
	now := time.Now()
	rec := mkRecs(1, time.Hour)[0]
	rec.Credits = map[string]any{"remain": 100}
	a := New([]*accounts.Record{rec}).All()[0]
	for i := 0; i < BreakerThreshold; i++ {
		a.NoteFailure("500", now)
	}
	if got := a.State(now); got != StatusCooling {
		t.Fatalf("breaker state = %s, want cooling", got)
	}
	if !strings.Contains(a.UnavailableReason("", now), "\u7194\u65ad") {
		t.Fatalf("breaker reason = %q", a.UnavailableReason("", now))
	}

	b := New([]*accounts.Record{mkRecs(1, time.Hour)[0]}).All()[0]
	b.NoteBalanceCooled("402", now)
	if got := b.State(now); got != StatusCooling {
		t.Fatalf("balance-park state = %s, want cooling", got)
	}
	if !strings.Contains(b.UnavailableReason("", now), "\u4f59\u989d\u4fdd\u62a4") {
		t.Fatalf("balance reason = %q", b.UnavailableReason("", now))
	}
}

func TestPickPrefersLeastLoadedAccount(t *testing.T) {
	p := New(mkRecs(3, time.Hour))
	busy := p.All()[0]
	busy.Acquire()
	defer busy.Release()
	for i := 0; i < 10; i++ {
		a := p.Pick("intl", "m", nil)
		if a == nil || a.UID == busy.UID {
			t.Fatalf("pick chose the busy account: %v", a)
		}
	}
}

func TestTopKSpreadsAcrossNearIdleAccounts(t *testing.T) {
	p := New(mkRecs(4, time.Hour))
	p.SetTopK(3)
	if got := p.TopK(); got != 3 {
		t.Fatalf("TopK = %d, want 3", got)
	}
	seen := map[string]int{}
	for i := 0; i < 100; i++ {
		a := p.Pick("intl", "m", nil)
		if a == nil {
			t.Fatal("pick returned nil")
		}
		seen[a.UID]++
	}
	if len(seen) < 2 {
		t.Fatalf("top-K always chose the same account: %v", seen)
	}

	p.SetTopK(0)
	if got := p.TopK(); got != 1 {
		t.Fatalf("TopK after clamp = %d, want 1", got)
	}
}

func TestTopKNeverPicksBusyAccount(t *testing.T) {
	p := New(mkRecs(4, time.Hour))
	p.SetTopK(3)
	busy := p.All()[0]
	busy.Acquire()
	defer busy.Release()
	for i := 0; i < 20; i++ {
		if a := p.Pick("intl", "m", nil); a == nil || a.UID == busy.UID {
			t.Fatalf("top-K chose the busy account: %v", a)
		}
	}
}
