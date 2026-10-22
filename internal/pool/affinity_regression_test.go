package pool

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// Hook stores block outside the backing store mutex, so synchronous Demote is
// demonstrably independent of both the queue and an in-flight Bind.
type blockingAffinityStore struct {
	*fakeAffinityStore
	recentHook func(context.Context, string) error
	bindHook   func(context.Context, string, string) error
	demoteHook func(context.Context, string, string) error
}

func (s *blockingAffinityStore) Recent(ctx context.Context, key string, limit int) ([]string, error) {
	if s.recentHook != nil {
		if err := s.recentHook(ctx, key); err != nil {
			return nil, err
		}
	}
	return s.fakeAffinityStore.Recent(ctx, key, limit)
}

func (s *blockingAffinityStore) Bind(ctx context.Context, key, uid string, ttl time.Duration, history int) error {
	if s.bindHook != nil {
		if err := s.bindHook(ctx, key, uid); err != nil {
			return err
		}
	}
	return s.fakeAffinityStore.Bind(ctx, key, uid, ttl, history)
}

func (s *blockingAffinityStore) Demote(ctx context.Context, key, uid string) error {
	if s.demoteHook != nil {
		if err := s.demoteHook(ctx, key, uid); err != nil {
			return err
		}
	}
	return s.fakeAffinityStore.Demote(ctx, key, uid)
}

func waitAffinitySignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for affinity operation")
	}
}

func drainAffinity(t *testing.T, s *SessionAffinity) {
	t.Helper()
	done := make(chan struct{})
	s.mu.Lock()
	ok := s.enqueueLocked(func() { close(done) })
	s.mu.Unlock()
	if !ok {
		t.Fatal("could not enqueue drain barrier")
	}
	waitAffinitySignal(t, done)
}

func TestAffinityNegativeAndFailureCache(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("failure=", fail), func(t *testing.T) {
			store := newFakeAffinityStore()
			if fail {
				store.recentErr = errors.New("unavailable")
			}
			s := NewSessionAffinity()
			s.SetStore(store)
			for i := 0; i < 5; i++ {
				if got := s.Recent("absent"); len(got) != 0 {
					t.Fatalf("absent recent = %v", got)
				}
			}
			if store.recentCalls != 1 {
				t.Fatalf("5 cached reads made %d store calls, want 1", store.recentCalls)
			}
			s.mu.Lock()
			s.cache["absent"].loadedAt = time.Now().Add(-2 * s.cacheTTL())
			s.mu.Unlock()
			s.Recent("absent")
			if store.recentCalls != 2 {
				t.Fatalf("expired negative cache calls = %d, want 2", store.recentCalls)
			}
			if fail && s.SyncStats().Errors != 2 {
				t.Fatalf("errors = %d, want 2", s.SyncStats().Errors)
			}
		})
	}
}

func TestAffinityConcurrentMissSingleflightAndCancellation(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	store := &blockingAffinityStore{fakeAffinityStore: newFakeAffinityStore()}
	store.recentHook = func(ctx context.Context, _ string) error {
		close(started) // a second read would panic
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s := NewSessionAffinity()
	s.Timeout = time.Second
	s.SetStore(store)
	leader := make(chan struct{})
	go func() { s.Recent("same"); close(leader) }()
	waitAffinitySignal(t, started)
	ctx, cancel := context.WithCancel(context.Background())
	waiter := make(chan struct{})
	go func() { s.RecentContext(ctx, "same"); close(waiter) }()
	cancel()
	waitAffinitySignal(t, waiter)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.Recent("same") }()
	}
	// Unrelated local work cannot be blocked by the store read.
	local := make(chan struct{})
	go func() { s.Bind("other", "warm"); close(local) }()
	waitAffinitySignal(t, local)
	close(release)
	waitAffinitySignal(t, leader)
	wg.Wait()
	drainAffinity(t, s)
	if store.recentCalls != 1 {
		t.Fatalf("concurrent reads = %d, want 1", store.recentCalls)
	}
}

func TestAffinityReadInitiatorCancellation(t *testing.T) {
	started, exited := make(chan struct{}), make(chan struct{})
	store := &blockingAffinityStore{fakeAffinityStore: newFakeAffinityStore()}
	store.recentHook = func(ctx context.Context, _ string) error {
		close(started)
		<-ctx.Done()
		close(exited)
		return ctx.Err()
	}
	s := NewSessionAffinity()
	s.SetStore(store)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.RecentContext(ctx, "cancel"); close(done) }()
	waitAffinitySignal(t, started)
	cancel()
	waitAffinitySignal(t, exited)
	waitAffinitySignal(t, done)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reads) != 0 {
		t.Fatalf("leaked in-flight reads: %d", len(s.reads))
	}
}

func TestAffinityCapacityIncludesNegativeMetadataAndLRU(t *testing.T) {
	s := NewSessionAffinity()
	s.MaxEntries = 5
	for i := 0; i < 5; i++ {
		s.Bind(fmt.Sprint(i), "warm")
	}
	s.Recent("0") // touch oldest; next admission must evict 1, not 0
	s.Bind("5", "warm")
	if s.Len() != 5 || s.cache["0"] == nil || s.cache["1"] != nil {
		t.Fatalf("capacity/LRU mismatch: len=%d", s.Len())
	}
	s.SetStore(newFakeAffinityStore())
	for i := 0; i < 100; i++ {
		s.Recent(fmt.Sprint("absent-", i))
	}
	if len(s.cache) != 5 || s.Len() != 0 {
		t.Fatalf("negative metadata not bounded: cache=%d bindings=%d", len(s.cache), s.Len())
	}
	for i := 0; i < 100; i++ {
		s.Demote(fmt.Sprint("demoted-", i), "failed")
	}
	if len(s.cache) != 5 || len(s.demotes) != 0 || len(s.reads) != 0 || len(s.writes) != 0 {
		t.Fatalf("metadata leaked: cache=%d demotes=%d reads=%d writes=%d", len(s.cache), len(s.demotes), len(s.reads), len(s.writes))
	}
}

func TestAffinityHistoryRetainsIndependentTTL(t *testing.T) {
	s := NewSessionAffinity()
	s.History = 3
	s.Bind("key", "old")
	s.Bind("key", "middle")
	s.Bind("key", "front")
	s.mu.Lock()
	e := s.cache["key"]
	oldExpiry := time.Now().Add(20 * time.Minute)
	e.entries[2].expires = oldExpiry
	e.entries[1].expires = time.Now().Add(-time.Second)
	s.mu.Unlock()
	s.Bind("key", "new")
	if got := s.Recent("key"); !slices.Equal(got, []string{"new", "front", "old"}) {
		t.Fatalf("history = %v", got)
	}
	if got := s.cache["key"].entries[2].expires; !got.Equal(oldExpiry) {
		t.Fatalf("warm account expiry changed: %v != %v", got, oldExpiry)
	}
	s.Demote("key", "front")
	if got := s.Recent("key"); !slices.Equal(got, []string{"new", "old"}) {
		t.Fatalf("demoted history = %v", got)
	}
}

func TestAffinityRefreshCannotUndoMutationOrEviction(t *testing.T) {
	for _, evict := range []bool{false, true} {
		t.Run(fmt.Sprint("evict=", evict), func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			store := &blockingAffinityStore{fakeAffinityStore: newFakeAffinityStore()}
			store.bindings["target"] = []string{"stale"}
			store.recentHook = func(ctx context.Context, _ string) error {
				close(started)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			s := NewSessionAffinity()
			s.MaxEntries = 1
			s.SetStore(store)
			done := make(chan struct{})
			go func() { s.Recent("target"); close(done) }()
			waitAffinitySignal(t, started)
			if evict {
				s.Bind("other", "warm")
			}
			s.Demote("target", "stale")
			// Keep the read stale even after the shared Demote.
			store.mu.Lock()
			store.bindings["target"] = []string{"stale"}
			store.mu.Unlock()
			close(release)
			waitAffinitySignal(t, done)
			if got := s.Recent("target"); len(got) != 0 {
				t.Fatalf("stale refresh undid demote: %v", got)
			}
			drainAffinity(t, s)
		})
	}
}

func TestAffinityQueuedBindCannotResurrectDemotedAccount(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	store := &blockingAffinityStore{fakeAffinityStore: newFakeAffinityStore()}
	store.bindHook = func(ctx context.Context, key, _ string) error {
		if key == "block" {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	s := NewSessionAffinity()
	s.Timeout = time.Second
	s.MaxEntries = 1
	s.SetStore(store)
	s.Bind("block", "other")
	waitAffinitySignal(t, started)
	s.Bind("target", "failed")
	s.Demote("target", "failed")
	store.mu.Lock()
	calls := store.demoteCalls
	store.mu.Unlock()
	if calls != 1 {
		t.Fatal("shared Demote was not immediate while worker blocked")
	}
	// Eviction must not discard the queued write's invalidation marker.
	s.Bind("evict", "warm")
	close(release)
	drainAffinity(t, s)
	store.mu.Lock()
	defer store.mu.Unlock()
	if got := store.bindings["target"]; len(got) != 0 {
		t.Fatalf("stale queued bind resurrected account: %v", got)
	}
}

func TestAffinityInflightBindRepairPrecedesNewerBind(t *testing.T) {
	for _, newer := range []bool{false, true} {
		t.Run(fmt.Sprint("newer=", newer), func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			store := &blockingAffinityStore{fakeAffinityStore: newFakeAffinityStore()}
			var once sync.Once
			store.bindHook = func(_ context.Context, _, _ string) error {
				// Model an already accepted write that ignores cancellation and
				// commits late. The test always explicitly releases it.
				once.Do(func() { close(started); <-release })
				return nil
			}
			s := NewSessionAffinity()
			s.SetStore(store)
			s.Bind("target", "failed")
			waitAffinitySignal(t, started)
			s.Demote("target", "failed")
			if newer {
				s.Bind("target", "failed")
			}
			close(release)
			drainAffinity(t, s)
			store.mu.Lock()
			defer store.mu.Unlock()
			want := []string(nil)
			if newer {
				want = []string{"failed"}
			}
			if got := store.bindings["target"]; !slices.Equal(got, want) {
				t.Fatalf("after repair/newer bind = %v, want %v", got, want)
			}
			if store.demoteCalls != 2 {
				t.Fatalf("immediate + repair Demote calls = %d, want 2", store.demoteCalls)
			}
		})
	}
}

func TestAffinityBindDuringDemoteWaitsForCompletion(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	store := &blockingAffinityStore{fakeAffinityStore: newFakeAffinityStore()}
	store.demoteHook = func(ctx context.Context, _, _ string) error {
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s := NewSessionAffinity()
	s.Timeout = time.Second
	s.SetStore(store)
	done := make(chan struct{})
	go func() { s.Demote("target", "account"); close(done) }()
	waitAffinitySignal(t, started)
	s.Bind("target", "account")
	store.mu.Lock()
	calls := store.bindCalls
	store.mu.Unlock()
	if calls != 0 {
		t.Fatal("Bind overtook active Demote")
	}
	close(release)
	waitAffinitySignal(t, done)
	drainAffinity(t, s)
	store.mu.Lock()
	defer store.mu.Unlock()
	if got := store.bindings["target"]; !slices.Equal(got, []string{"account"}) {
		t.Fatalf("newer bind was removed by older Demote: %v", got)
	}
}

func TestAffinityWorkerDrainsWithoutPermanentGoroutine(t *testing.T) {
	s := NewSessionAffinity()
	if s.running {
		t.Fatal("constructor started a permanent worker")
	}
	s.SetStore(newFakeAffinityStore())
	s.Bind("key", "account")
	drainAffinity(t, s)
	s.mu.Lock()
	done := s.workerDone
	s.mu.Unlock()
	waitAffinitySignal(t, done)
	s.mu.Lock()
	if s.running || len(s.writes) != 0 {
		t.Fatal("drained worker or write metadata leaked")
	}
	s.mu.Unlock()
	// Explicitly exercise restart after the previous worker has exited.
	s.Bind("key", "other")
	drainAffinity(t, s)
	s.mu.Lock()
	done = s.workerDone
	s.mu.Unlock()
	waitAffinitySignal(t, done)
}

func BenchmarkAffinityBindBounded(b *testing.B) {
	for _, n := range []int{4999, 5000, 20000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			s := NewSessionAffinity()
			// Unlike the old audit probe's direct map injection, construct the
			// cache through the public API and allow all n live conversations.
			s.MaxEntries = max(5000, n)
			for i := 0; i < n; i++ {
				s.Bind(fmt.Sprint(i), "acct")
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s.Bind("0", "acct")
			}
		})
	}
}
