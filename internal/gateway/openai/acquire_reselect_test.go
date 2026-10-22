package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/store/accounts"
)

func reselectAccounts() (*pool.Account, *pool.Account) {
	return pool.NewAccount(&accounts.Record{UID: "a", AccessToken: "token", Weight: 100}),
		pool.NewAccount(&accounts.Record{UID: "b", AccessToken: "token"})
}

func reselectRequest(ctx context.Context, candidates ...*pool.Account) *request {
	q := &request{r: httptest.NewRequest("POST", "/", nil).WithContext(ctx), model: "model", turnID: "turn"}
	q.at.full = make(map[string]*pool.Account)
	q.at.tried = make(map[string]struct{})
	for _, a := range candidates {
		q.at.full[a.UID] = a
	}
	return q
}

type reselectResult struct {
	account *pool.Account
	release func()
	ok      bool
}

func startReselect(h *Handler, q *request) <-chan reselectResult {
	result := make(chan reselectResult, 1)
	go func() {
		a, release, ok := h.waitForFullAccount(q)
		result <- reselectResult{a, release, ok}
	}()
	return result
}

func receiveReselect(t *testing.T, result <-chan reselectResult) reselectResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("account wait did not complete")
		return reselectResult{}
	}
}

func assertReselectAttempt(t *testing.T, q *request, uid string) {
	t.Helper()
	if q.at.charged != nil || q.at.skipped != 0 || q.at.transient != 0 {
		t.Fatal("capacity wait spent a failure attempt")
	}
	if uid == "" {
		if len(q.at.tried) != 0 {
			t.Fatalf("failed wait marked tried: %v", q.at.tried)
		}
	} else if len(q.at.tried) != 1 || q.at.lastUID != uid || q.at.allFull {
		t.Fatalf("admitted attempt bookkeeping = %+v", q.at)
	}
}

// AccountScore lets tests pause exactly after the wait began ranking candidates,
// rather than sleep and assume the waiting goroutine has started.
type reselectFeedback struct {
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (*reselectFeedback) ShouldEscape(string) bool { return false }
func (s *reselectFeedback) Score(string) (float64, float64, bool) {
	s.once.Do(func() { close(s.started); <-s.release })
	return 0, 0, false
}

func TestAccountWaitLocalAlternateFreesAndBestTurnsUnhealthy(t *testing.T) {
	for _, unhealthy := range []bool{false, true} {
		t.Run(fmt.Sprint("unhealthy=", unhealthy), func(t *testing.T) {
			a, b := reselectAccounts()
			a.SetMaxInflight(1)
			b.SetMaxInflight(1)
			releaseA, _ := a.TryAcquire(0)
			releaseB, _ := b.TryAcquire(0)
			defer releaseA()
			defer releaseB()
			feedback := &reselectFeedback{started: make(chan struct{}), release: make(chan struct{})}
			h := &Handler{AccountWait: time.Second, Sched: feedback}
			q := reselectRequest(context.Background(), a, b)
			done := startReselect(h, q)
			<-feedback.started
			if unhealthy {
				a.SetEnabled(false)
			}
			releaseB()
			close(feedback.release)
			got := receiveReselect(t, done)
			if !got.ok || got.account != b {
				t.Fatalf("alternate free account not selected: %+v", got)
			}
			assertReselectAttempt(t, q, b.UID)
			got.release()
			got.release()
			if a.Inflight() != 1 || b.Inflight() != 0 {
				t.Fatalf("local reservation leak: a=%d b=%d", a.Inflight(), b.Inflight())
			}
		})
	}
}

type reselectSharedLimiter struct {
	mu            sync.Mutex
	busy          map[string]bool
	calls         []string
	budgets       []time.Duration
	ctxs          []context.Context
	releases      int
	first         chan struct{}
	proceed       chan struct{}
	once          sync.Once
	hook          func(string)
	immediateDeny bool
}

func (s *reselectSharedLimiter) Acquire(context.Context, string, int, string) (func(), bool, error) {
	return nil, false, errors.New("wait path must use AcquireWait")
}

func (s *reselectSharedLimiter) AcquireWait(ctx context.Context, uid string, _ int, _ string, budget time.Duration) (func(), bool, error) {
	s.mu.Lock()
	s.calls = append(s.calls, uid)
	s.budgets = append(s.budgets, budget)
	s.ctxs = append(s.ctxs, ctx)
	s.mu.Unlock()
	s.once.Do(func() {
		if s.first != nil {
			close(s.first)
			select {
			case <-s.proceed:
			case <-ctx.Done():
			}
		}
	})
	s.mu.Lock()
	busy := s.busy[uid]
	s.mu.Unlock()
	if s.hook != nil {
		s.hook(uid)
	}
	if !busy {
		return func() { s.mu.Lock(); s.releases++; s.mu.Unlock() }, true, nil
	}
	if s.immediateDeny {
		return nil, false, nil
	}
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil, false, nil
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
}

func TestAccountWaitSharedAlternateFreesKeepsLeaseContext(t *testing.T) {
	a, b := reselectAccounts()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lim := &reselectSharedLimiter{busy: map[string]bool{"a": true, "b": true}, first: make(chan struct{}), proceed: make(chan struct{})}
	h := &Handler{AccountWait: time.Second, AccountInflight: 1, Limiter: lim}
	q := reselectRequest(ctx, a, b)
	done := startReselect(h, q)
	<-lim.first
	lim.mu.Lock()
	lim.busy[b.UID] = false
	lim.mu.Unlock()
	close(lim.proceed)
	got := receiveReselect(t, done)
	if !got.ok || got.account != b {
		t.Fatalf("shared alternate not selected: %+v", got)
	}
	assertReselectAttempt(t, q, b.UID)
	lim.mu.Lock()
	for i, observed := range lim.ctxs {
		if observed != ctx || observed.Err() != nil || lim.budgets[i] > 10*time.Millisecond {
			t.Fatal("probe context truncated successful lease or exceeded quantum")
		}
	}
	lim.mu.Unlock()
	got.release()
	got.release()
	lim.mu.Lock()
	releases := lim.releases
	lim.mu.Unlock()
	if releases != 1 || a.Inflight() != 0 || b.Inflight() != 0 {
		t.Fatalf("release count=%d local a=%d b=%d", releases, a.Inflight(), b.Inflight())
	}
}

func TestAccountWaitShortBudgetLeavesAlternateProbeTime(t *testing.T) {
	a, b := reselectAccounts()
	lim := &reselectSharedLimiter{busy: map[string]bool{"a": true}, immediateDeny: true}
	h := &Handler{AccountWait: 9 * time.Millisecond, AccountInflight: 1, Limiter: lim}
	q := reselectRequest(context.Background(), a, b)
	// full is authoritative for this phase, including a current pinned/local
	// attempt that a caller may already have inserted into tried.
	q.at.tried[a.UID] = struct{}{}
	got, release, ok := h.waitForFullAccount(q)
	if !ok || got != b {
		t.Fatalf("short budget did not admit free alternate: %v", got)
	}
	release()
	if len(lim.calls) != 2 || lim.calls[0] != a.UID || lim.budgets[0] > h.AccountWait/2 {
		t.Fatalf("first busy candidate consumed alternate's budget: calls=%v budgets=%v", lim.calls, lim.budgets)
	}
	if a.Inflight() != 0 || b.Inflight() != 0 || lim.releases != 1 {
		t.Fatal("short-budget reservations leaked")
	}
}

func TestAccountWaitPinnedNeverSelectsAlternate(t *testing.T) {
	a, b := reselectAccounts()
	a.SetMaxInflight(1)
	release, _ := a.TryAcquire(0)
	defer release()
	ctx := context.WithValue(context.Background(), pinnedKey{}, a)
	h := &Handler{AccountWait: 20 * time.Millisecond}
	q := reselectRequest(ctx, a, b) // deliberately polluted full set
	if _, _, ok := h.waitForFullAccount(q); ok {
		t.Fatal("pinned request escaped to free alternate")
	}
	assertReselectAttempt(t, q, "")
	if a.Inflight() != 1 || b.Inflight() != 0 {
		t.Fatal("pinned wait changed reservations")
	}
}

func TestAccountWaitPinnedCurrentAttemptCanAcquire(t *testing.T) {
	a, b := reselectAccounts()
	ctx := context.WithValue(context.Background(), pinnedKey{}, a)
	h := &Handler{AccountWait: time.Second, AccountInflight: 1}
	q := reselectRequest(ctx, a, b)
	q.at.tried[a.UID] = struct{}{}
	got, release, ok := h.waitForFullAccount(q)
	if !ok || got != a {
		t.Fatalf("current pinned attempt excluded by tried: %v", got)
	}
	release()
	assertReselectAttempt(t, q, a.UID)
	if a.Inflight() != 0 || b.Inflight() != 0 {
		t.Fatal("pinned current attempt reservation leaked")
	}
}

type reselectDeadlineLimiter struct {
	wait        time.Duration
	lateSuccess bool
	calls       []string
	budgets     []time.Duration
	ctxs        []context.Context
	releases    int
}

func (*reselectDeadlineLimiter) Acquire(context.Context, string, int, string) (func(), bool, error) {
	return nil, false, errors.New("wait path must use AcquireWait")
}

func (s *reselectDeadlineLimiter) AcquireWait(ctx context.Context, uid string, _ int, _ string, budget time.Duration) (func(), bool, error) {
	s.calls = append(s.calls, uid)
	s.budgets = append(s.budgets, budget)
	s.ctxs = append(s.ctxs, ctx)
	if len(s.calls) == 1 {
		// Deliberately exceed the admission budget AND the original overall
		// wait measured from this call's entry. This tests the caller's hard
		// absolute fence, independent of OS timer wake-up/scheduling latency.
		timer := time.NewTimer(s.wait)
		defer timer.Stop()
		<-timer.C
		if !s.lateSuccess {
			return nil, false, nil
		}
	}
	// Any second probe would succeed: resetting the deadline per candidate
	// must not escape the original overall fence by admitting that account.
	return func() { s.releases++ }, true, nil
}

func TestAccountWaitSharedDeadlineIsOverallNotPerCandidate(t *testing.T) {
	for _, lateSuccess := range []bool{false, true} {
		t.Run(fmt.Sprint("lateSuccess=", lateSuccess), func(t *testing.T) {
			a, b := reselectAccounts()
			// One second leaves ample pre-entry scheduling headroom; the test
			// asserts behavior after expiry, not an elapsed-time upper bound.
			lim := &reselectDeadlineLimiter{wait: time.Second, lateSuccess: lateSuccess}
			h := &Handler{AccountWait: lim.wait, AccountInflight: 1, Limiter: lim}
			ctx := context.Background()
			q := reselectRequest(ctx, a, b)
			got, release, ok := h.waitForFullAccount(q)
			if release != nil {
				release()
			}
			if ok || got != nil || release != nil {
				t.Fatal("returned admission or lease after original overall deadline")
			}
			if len(lim.calls) != 1 || lim.calls[0] != a.UID {
				t.Fatalf("deadline reset or expired before first probe: calls=%v", lim.calls)
			}
			if lim.ctxs[0] != ctx || lim.budgets[0] <= 0 || lim.budgets[0] > 10*time.Millisecond || lim.budgets[0] > h.AccountWait/2 {
				t.Fatalf("probe changed lease context or exceeded bounded share: budgets=%v", lim.budgets)
			}
			wantReleases := 0
			if lateSuccess {
				wantReleases = 1
			}
			if lim.releases != wantReleases || a.Inflight() != 0 || b.Inflight() != 0 {
				t.Fatalf("late response leaked reservations: releases=%d want=%d local a=%d b=%d", lim.releases, wantReleases, a.Inflight(), b.Inflight())
			}
			assertReselectAttempt(t, q, "")
		})
	}
}

func TestAccountWaitSharedCancellationReleasesReservation(t *testing.T) {
	a, b := reselectAccounts()
	ctx, cancel := context.WithCancel(context.Background())
	lim := &reselectSharedLimiter{busy: map[string]bool{"a": true, "b": true}, first: make(chan struct{}), proceed: make(chan struct{})}
	h := &Handler{AccountWait: time.Second, AccountInflight: 1, Limiter: lim}
	q := reselectRequest(ctx, a, b)
	done := startReselect(h, q)
	<-lim.first
	cancel()
	if got := receiveReselect(t, done); got.ok {
		t.Fatal("canceled shared wait admitted")
	}
	if a.Inflight() != 0 || b.Inflight() != 0 {
		t.Fatal("canceled wait leaked local reservation")
	}
	assertReselectAttempt(t, q, "")
}

func TestAccountWaitHealthChangesAfterSharedAdmission(t *testing.T) {
	a, b := reselectAccounts()
	lim := &reselectSharedLimiter{busy: map[string]bool{}, hook: func(uid string) {
		if uid == a.UID {
			a.SetEnabled(false)
		}
	}}
	h := &Handler{AccountWait: time.Second, AccountInflight: 1, Limiter: lim}
	q := reselectRequest(context.Background(), a, b)
	got, release, ok := h.waitForFullAccount(q)
	if !ok || got != b {
		t.Fatalf("did not escape newly unhealthy best: %v", got)
	}
	release()
	release()
	if lim.releases != 2 || a.Inflight() != 0 || b.Inflight() != 0 {
		t.Fatalf("unhealthy reservation not cleaned: releases=%d", lim.releases)
	}
	assertReselectAttempt(t, q, b.UID)
}

type reselectLegacyLimiter struct {
	ctx      context.Context
	releases int
}

func (s *reselectLegacyLimiter) Acquire(ctx context.Context, _ string, _ int, _ string) (func(), bool, error) {
	s.ctx = ctx
	time.Sleep(20 * time.Millisecond) // interface has no admission-only budget
	return func() { s.releases++ }, true, nil
}

func TestAccountWaitLegacyLateSuccessIsReleased(t *testing.T) {
	a, _ := reselectAccounts()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lim := &reselectLegacyLimiter{}
	h := &Handler{AccountWait: time.Millisecond, AccountInflight: 1, Limiter: lim}
	q := reselectRequest(ctx, a)
	if _, _, ok := h.waitForFullAccount(q); ok {
		t.Fatal("late legacy success admitted past deadline")
	}
	if lim.ctx != ctx || lim.releases != 1 || a.Inflight() != 0 {
		t.Fatal("legacy lease context replaced or late reservation leaked")
	}
	assertReselectAttempt(t, q, "")
}
