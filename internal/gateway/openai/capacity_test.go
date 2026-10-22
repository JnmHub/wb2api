package openai

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/store/accounts"
)

type capacityFake struct {
	mu                        sync.Mutex
	counts                    map[string]int
	stale                     map[string]int
	batches, probes, releases int
	delay                     time.Duration
	err                       error
	onProbe                   func(string)
	onBatch                   func(int)
}

func (f *capacityFake) Occupancy(ctx context.Context, ids []string, fresh bool) (map[string]int, error) {
	f.mu.Lock()
	f.batches++
	batch := f.batches
	f.mu.Unlock()
	if f.onBatch != nil {
		f.onBatch(batch)
	}
	if f.delay > 0 {
		timer := time.NewTimer(f.delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	src := f.counts
	if !fresh && f.stale != nil {
		src = f.stale
	}
	out := make(map[string]int, len(ids))
	for _, id := range ids {
		out[id] = src[id]
	}
	return out, nil
}
func (f *capacityFake) Acquire(ctx context.Context, uid string, max int, member string) (func(), bool, error) {
	return f.AcquireWithin(ctx, uid, max, member, time.Second)
}
func (f *capacityFake) AcquireWithin(ctx context.Context, uid string, max int, _ string, _ time.Duration) (func(), bool, error) {
	if f.onProbe != nil {
		f.onProbe(uid)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probes++
	if f.err != nil {
		return nil, false, f.err
	}
	if ctx.Err() != nil {
		return nil, false, ctx.Err()
	}
	if f.counts[uid] >= max {
		return nil, false, nil
	}
	f.counts[uid]++
	var once sync.Once
	return func() { once.Do(func() { f.mu.Lock(); defer f.mu.Unlock(); f.counts[uid]--; f.releases++ }) }, true, nil
}
func capacityHandler(n int) (*Handler, *capacityFake, *request) {
	recs := make([]*accounts.Record, n)
	counts := make(map[string]int, n)
	for i := range recs {
		id := fmt.Sprintf("a%04d", i)
		recs[i] = &accounts.Record{UID: id, Realm: "intl", AccessToken: "token"}
		counts[id] = 1
	}
	f := &capacityFake{counts: counts, delay: 2 * time.Millisecond}
	h := &Handler{Pool: pool.New(recs), Limiter: f, AccountInflight: 1}
	q := &request{r: httptest.NewRequest("POST", "/", nil), realm: "intl", model: "model", turnID: "turn"}
	q.at.tried = map[string]struct{}{}
	return h, f, q
}
func TestCapacitySaturatedBatchProbesBounded(t *testing.T) {
	for _, n := range []int{100, 1000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			h, f, q := capacityHandler(n)
			if a := h.pickAccount(q); a != nil {
				t.Fatalf("saturated selected %s", a.UID)
			}
			if f.batches > 2 || f.probes != 0 || len(q.at.full) != n || !q.at.allFull {
				t.Fatalf("N=%d batches=%d atomics=%d full=%d", n, f.batches, f.probes, len(q.at.full))
			}
			t.Logf("N=%d 2ms hint I/O batches=%d atomic probes=%d", n, f.batches, f.probes)
		})
	}
}
func TestCapacityStaleFullFreshRetryWithoutWait(t *testing.T) {
	h, f, q := capacityHandler(100)
	f.stale = make(map[string]int, len(f.counts))
	for id, n := range f.counts {
		f.stale[id] = n
	}
	f.counts["a0099"] = 0
	a := h.pickAccount(q)
	if a == nil || a.UID != "a0099" {
		t.Fatalf("released account wedged: %v", a)
	}
	rel, ok, err := h.acquireAccount(q, a)
	if err != nil || !ok {
		t.Fatalf("fresh free failed %v %v", ok, err)
	}
	rel()
	rel()
	if f.batches != 2 || f.probes != 1 || f.releases != 1 {
		t.Fatalf("batches/probes/releases=%d/%d/%d", f.batches, f.probes, f.releases)
	}
}
func TestCapacityWaitRefreshesAndFindsReleasedAlternate(t *testing.T) {
	h, f, q := capacityHandler(100)
	h.AccountWait = time.Second
	if h.pickAccount(q) != nil {
		t.Fatal("initial saturated")
	}
	f.onBatch = func(n int) {
		if n == 3 {
			f.mu.Lock()
			f.counts["a0099"] = 0
			f.mu.Unlock()
		}
	}
	a, rel, ok := h.waitForFullAccount(q)
	if !ok || a.UID != "a0099" {
		t.Fatalf("released alternate not found: %v", a)
	}
	rel()
	if f.probes != 1 || f.batches != 3 {
		t.Fatalf("wait serialized full atomics: %d/%d", f.probes, f.batches)
	}
}
func TestCapacityPinnedCannotEscapeBatchHints(t *testing.T) {
	h, f, q := capacityHandler(2)
	h.AccountWait = 20 * time.Millisecond
	f.counts["a0001"] = 0
	a := h.Pool.Get("a0000")
	q.r = q.r.WithContext(context.WithValue(q.r.Context(), pinnedKey{}, a))
	q.at.full = map[string]*pool.Account{"a0000": a, "a0001": h.Pool.Get("a0001")}
	if _, _, ok := h.waitForFullAccount(q); ok {
		t.Fatal("escaped pinned owner")
	}
	if f.probes != 0 {
		t.Fatal("probed full owner or alternate")
	}
}
func TestCapacityHintsNeverBypassAtomicCap(t *testing.T) {
	h, f, q := capacityHandler(1)
	f.stale = map[string]int{"a0000": 0}
	a := h.pickAccount(q)
	if a == nil {
		t.Fatal("need stale-free candidate")
	}
	rel, ok, err := h.acquireAccount(q, a)
	if rel != nil {
		rel()
	}
	if err != nil || ok {
		t.Fatal("stale-free hint bypassed atomic cap")
	}
	h.noteAccountFull(q, a)
	if h.pickAccount(q) != nil {
		t.Fatal("atomic denial not respected")
	}
}
func TestCapacityConcurrentStaleFreeStillAtomic(t *testing.T) {
	h, f, _ := capacityHandler(1)
	f.counts["a0000"] = 0
	f.stale = map[string]int{"a0000": 0}
	var wg sync.WaitGroup
	releases := make(chan func(), 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			q := reselectRequest(context.Background())
			q.realm, q.turnID = "intl", fmt.Sprint(i)
			a := h.pickAccount(q)
			if a == nil {
				return
			}
			rel, ok, err := h.acquireAccount(q, a)
			if err != nil {
				t.Error(err)
				return
			}
			if ok {
				releases <- rel
			}
		}(i)
	}
	wg.Wait()
	close(releases)
	if len(releases) != 1 {
		t.Fatalf("concurrent admissions=%d want1", len(releases))
	}
	for release := range releases {
		release()
	}
	if f.counts["a0000"] != 0 {
		t.Fatal("concurrent lease leaked")
	}
}

func TestCapacityWaitRefreshesAfterAtomicDenial(t *testing.T) {
	h, f, q := capacityHandler(1)
	h.AccountWait = 100 * time.Millisecond
	f.counts["a0000"] = 0
	q.at.full = map[string]*pool.Account{"a0000": h.Pool.Get("a0000")}
	f.onProbe = func(uid string) {
		if f.probes == 0 {
			f.counts[uid] = 1
		}
	}
	f.onBatch = func(n int) {
		if n == 2 {
			f.counts["a0000"] = 0
		}
	}
	_, rel, ok := h.waitForFullAccount(q)
	if !ok {
		t.Fatalf("atomic denied after free hint wedged single account: batches=%d probes=%d", f.batches, f.probes)
	}
	rel()
	if f.counts["a0000"] != 0 {
		t.Fatal("wait lease leaked")
	}
}

func TestCapacityCancelAndSingleBudget(t *testing.T) {
	h, f, q := capacityHandler(1000)
	h.AccountWait = 10 * time.Millisecond
	f.delay = time.Second
	h.startAdmission(q)
	deadline := q.at.admissionDeadline
	if a := h.pickAccount(q); a != nil {
		t.Fatal("admitted after hint deadline")
	}
	if _, _, ok := h.waitForFullAccount(q); ok {
		t.Fatal("wait reset admission deadline")
	}
	if !q.at.admissionDeadline.Equal(deadline) || f.probes != 0 {
		t.Fatal("initial budget not shared")
	}
	h, f, q = capacityHandler(100)
	ctx, cancel := context.WithCancel(q.r.Context())
	cancel()
	q.r = q.r.WithContext(ctx)
	if h.pickAccount(q) != nil || f.probes != 0 {
		t.Fatal("canceled request admitted")
	}
}
func TestCapacityPaidFallbackDoesNotBypassSharedCap(t *testing.T) {
	up := newRawUpstream(t, func(_ int, _ rawReq, c net.Conn) { sseOK(c) })
	f := newFixture(t, up, accSpec{}, accSpec{})
	f.setSettings(t, `{"free_model_credit_fallback":true}`)
	for _, a := range f.pool.All() {
		a.NoteModelCap("deepseek-v4.1-flash", time.Now().Add(time.Hour), time.Now())
	}
	lim := &capacityFake{counts: map[string]int{"acct-0": 1, "acct-1": 1}}
	f.h.Limiter, f.h.AccountInflight = lim, 1
	code, body, _ := f.post(t, `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"x"}]}`, nil)
	if code != 429 {
		t.Fatalf("paid fallback cap status=%d %s", code, body)
	}
	if lim.probes != 0 || len(up.requests()) != 0 {
		t.Fatal("paid fallback bypassed shared cap")
	}
}

func TestCapacityFallbackDoesNotBypassSharedCap(t *testing.T) {
	up := newRawUpstream(t, func(_ int, _ rawReq, c net.Conn) { sseOK(c) })
	f := newFixture(t, up, accSpec{}, accSpec{})
	lim := &capacityFake{counts: map[string]int{"acct-0": 1, "acct-1": 1}}
	f.h.Limiter = lim
	f.h.AccountInflight = 1
	code, body, _ := f.post(t, conv(1), nil)
	if code != 429 {
		t.Fatalf("capacity fallback status=%d %s", code, body)
	}
	if lim.probes != 0 || len(up.requests()) != 0 {
		t.Fatal("capacity hint fallback dispatched without reservation")
	}
}
func TestCapacityPinnedSharedFullOneProbe(t *testing.T) {
	up := newRawUpstream(t, func(_ int, _ rawReq, c net.Conn) { sseOK(c) })
	f := newFixture(t, up, accSpec{}, accSpec{}, accSpec{})
	lim := &capacityFake{counts: map[string]int{"acct-0": 1}}
	f.h.Limiter, f.h.AccountInflight = lim, 1
	srv := httptest.NewServer(PinnedRoutes(f.pool, f.h))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/u/acct-0/v1/chat/completions", "application/json", strings.NewReader(conv(1)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 429 || lim.probes != 1 || len(up.requests()) != 0 {
		t.Fatalf("owner denial must stop status=%d probes=%d reqs=%d", resp.StatusCode, lim.probes, len(up.requests()))
	}
}

func TestCapacityHealthChangesDuringAdmission(t *testing.T) {
	up := newRawUpstream(t, func(_ int, _ rawReq, c net.Conn) { sseOK(c) })
	f := newFixture(t, up, accSpec{}, accSpec{})
	lim := &capacityFake{counts: map[string]int{}}
	var guarded string
	lim.onProbe = func(uid string) {
		if guarded == "" {
			guarded = uid
			f.pool.Get(uid).SetGuards(pool.GuardState{DailyToken: true})
		}
	}
	f.h.Limiter, f.h.AccountInflight = lim, 1
	code, body, _ := f.post(t, conv(1), nil)
	if code != 200 {
		t.Fatalf("healthy alternate status=%d %s", code, body)
	}
	if len(up.requests()) != 1 || lim.releases != 2 || lim.counts[guarded] != 0 || f.pool.Get(guarded).Inflight() != 0 {
		t.Fatalf("stale health dispatched or slots leaked reqs=%d releases=%d", len(up.requests()), lim.releases)
	}
}

func TestCapacityHintFailurePolicy(t *testing.T) {
	for _, failOpen := range []bool{false, true} {
		t.Run(fmt.Sprint(failOpen), func(t *testing.T) {
			h, f, q := capacityHandler(1)
			h.SharedLimitFailOpen = failOpen
			f.err = errors.New("redis offline")
			q.w = httptest.NewRecorder()
			a := h.pickAccount(q)
			if failOpen {
				if a == nil {
					t.Fatal("explicit failopen should retain legacy selection")
				}
			} else if a != nil || !h.admissionFailed(q) {
				t.Fatal("strict error did not fail closed")
			}
		})
	}
}
