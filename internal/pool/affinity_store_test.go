package pool

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeAffinityStore struct {
	mu          sync.Mutex
	bindings    map[string][]string
	recentCalls int
	bindCalls   int
	demoteCalls int
	recentErr   error
}

func newFakeAffinityStore() *fakeAffinityStore {
	return &fakeAffinityStore{bindings: map[string][]string{}}
}

func (f *fakeAffinityStore) Recent(_ context.Context, key string, limit int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recentCalls++
	if f.recentErr != nil {
		return nil, f.recentErr
	}
	got := append([]string(nil), f.bindings[key]...)
	if limit > 0 && len(got) > limit {
		got = got[:limit]
	}
	return got, nil
}

func (f *fakeAffinityStore) Bind(_ context.Context, key, uid string, _ time.Duration, history int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bindCalls++
	next := []string{uid}
	for _, old := range f.bindings[key] {
		if old != uid {
			next = append(next, old)
		}
	}
	if history > 0 && len(next) > history {
		next = next[:history]
	}
	f.bindings[key] = next
	return nil
}

func (f *fakeAffinityStore) Demote(_ context.Context, key, uid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.demoteCalls++
	var kept []string
	for _, old := range f.bindings[key] {
		if old != uid {
			kept = append(kept, old)
		}
	}
	if len(kept) == 0 {
		delete(f.bindings, key)
	} else {
		f.bindings[key] = kept
	}
	return nil
}

func (f *fakeAffinityStore) waitForBinds(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		got := f.bindCalls
		f.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d store binds", n)
}

func TestAffinitySharesBindingsBetweenReplicas(t *testing.T) {
	store := newFakeAffinityStore()
	a := NewSessionAffinity()
	a.SetStore(store)
	a.Bind("sess", "acct-1")
	store.waitForBinds(t, 1)

	b := NewSessionAffinity()
	b.SetStore(store)
	if got := b.Get("sess"); got != "acct-1" {
		t.Fatalf("replica b got %q, want acct-1", got)
	}
}

func TestAffinityDemoteIsVisibleToOtherReplicas(t *testing.T) {
	store := newFakeAffinityStore()
	store.bindings["sess"] = []string{"acct-1", "acct-0"}

	a := NewSessionAffinity()
	a.SetStore(store)
	a.Demote("sess", "acct-1")

	b := NewSessionAffinity()
	b.SetStore(store)
	if got := b.Recent("sess"); len(got) != 1 || got[0] != "acct-0" {
		t.Fatalf("replica b recent = %v, want [acct-0]", got)
	}
}

func TestAffinityCachesStoreReads(t *testing.T) {
	store := newFakeAffinityStore()
	store.bindings["sess"] = []string{"acct-1"}
	a := NewSessionAffinity()
	a.SetStore(store)
	if got := a.Recent("sess"); len(got) != 1 || got[0] != "acct-1" {
		t.Fatalf("recent = %v", got)
	}
	a.Recent("sess")
	store.mu.Lock()
	calls := store.recentCalls
	store.mu.Unlock()
	if calls != 1 {
		t.Fatalf("store read %d times, want 1 (cache miss only)", calls)
	}
}

func TestAffinityBindIsLocalBeforeStoreWrite(t *testing.T) {
	store := newFakeAffinityStore()
	a := NewSessionAffinity()
	a.SetStore(store)
	a.Bind("sess", "acct-1")
	if got := a.Get("sess"); got != "acct-1" {
		t.Fatalf("local get = %q, want acct-1", got)
	}
}

func TestAffinitySyncStatsReportsSharedMode(t *testing.T) {
	store := newFakeAffinityStore()
	a := NewSessionAffinity()
	a.SetStore(store)
	a.Bind("sess", "acct-1")
	store.waitForBinds(t, 1)
	stats := a.SyncStats()
	if !stats.Shared || stats.Pending != 0 || stats.Dropped != 0 || stats.Errors != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestAffinityRefreshesAfterCacheTTL(t *testing.T) {
	store := newFakeAffinityStore()
	store.bindings["sess"] = []string{"acct-1"}
	a := NewSessionAffinity()
	a.CacheTTL = 15 * time.Millisecond
	a.SetStore(store)
	if got := a.Recent("sess"); len(got) != 1 || got[0] != "acct-1" {
		t.Fatalf("first recent = %v", got)
	}
	store.mu.Lock()
	store.bindings["sess"] = []string{"acct-2"}
	store.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	if got := a.Recent("sess"); len(got) != 1 || got[0] != "acct-2" {
		t.Fatalf("recent after TTL = %v", got)
	}
}

func TestAffinityStoreErrorFallsBackToLocal(t *testing.T) {
	store := newFakeAffinityStore()
	a := NewSessionAffinity()
	a.CacheTTL = time.Millisecond
	a.SetStore(store)
	a.Bind("sess", "acct-local")
	store.waitForBinds(t, 1)
	store.mu.Lock()
	store.recentErr = errors.New("redis down")
	store.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	if got := a.Recent("sess"); len(got) != 1 || got[0] != "acct-local" {
		t.Fatalf("recent with store error = %v", got)
	}
	if stats := a.SyncStats(); stats.Errors == 0 {
		t.Fatalf("store error was not counted: %+v", stats)
	}
}
