package pool

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestAffinityBindQueueIsBoundedAndDemoteStillImmediate(t *testing.T) {
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
	s.Timeout = 2 * time.Second
	s.SetStore(store)
	s.Bind("block", "other")
	waitAffinitySignal(t, started)
	for i := 0; i < cap(s.syncCh)+10; i++ {
		s.Bind(fmt.Sprint(i), "failed")
	}
	stats := s.SyncStats()
	if stats.Pending != cap(s.syncCh) || stats.Dropped != 10 {
		t.Fatalf("queue stats = %+v", stats)
	}
	s.mu.Lock()
	writes := 0
	for _, pending := range s.writes {
		writes += len(pending)
	}
	s.mu.Unlock()
	if writes != cap(s.syncCh)+1 {
		t.Fatalf("write metadata grew past queue + executing write: %d", writes)
	}
	s.Demote("0", "failed")
	store.mu.Lock()
	calls := store.demoteCalls
	store.mu.Unlock()
	if calls != 1 {
		t.Fatal("Demote queued behind full Bind queue")
	}
	s.mu.Lock()
	done := s.workerDone
	s.mu.Unlock()
	close(release)
	waitAffinitySignal(t, done)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.writes) != 0 || s.running {
		t.Fatal("worker/write state leaked after overload")
	}
}

func TestAffinityStoreTimeoutReleasesWorker(t *testing.T) {
	store := &blockingAffinityStore{fakeAffinityStore: newFakeAffinityStore()}
	store.bindHook = func(ctx context.Context, _, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	s := NewSessionAffinity()
	s.Timeout = time.Millisecond
	s.SetStore(store)
	s.Bind("key", "account")
	s.mu.Lock()
	done := s.workerDone
	s.mu.Unlock()
	waitAffinitySignal(t, done)
	if s.SyncStats().Errors != 1 {
		t.Fatalf("timeout errors = %d", s.SyncStats().Errors)
	}
}

func TestAffinityGetSlidesOnlyFrontAndExpiryIsLazy(t *testing.T) {
	s := NewSessionAffinity()
	s.Bind("key", "warm")
	s.Bind("key", "front")
	s.mu.Lock()
	e := s.cache["key"]
	oldExpiry := time.Now().Add(time.Minute)
	e.entries[0].expires = oldExpiry
	e.entries[1].expires = oldExpiry
	s.mu.Unlock()
	if got := s.Get("key"); got != "front" {
		t.Fatalf("get = %q", got)
	}
	if !e.entries[0].expires.After(oldExpiry) || !e.entries[1].expires.Equal(oldExpiry) {
		t.Fatal("Get did not slide exactly the front account")
	}
	s.mu.Lock()
	for i := range e.entries {
		e.entries[i].expires = time.Now().Add(-time.Second)
	}
	s.mu.Unlock()
	if got := s.Recent("key"); len(got) != 0 || s.Len() != 0 {
		t.Fatalf("expired history survived: %v", got)
	}
}

func TestAffinityReadConcurrencyBound(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	store := &blockingAffinityStore{fakeAffinityStore: newFakeAffinityStore()}
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
	go func() { s.Recent("blocked"); close(done) }()
	waitAffinitySignal(t, started)
	// A second distinct key must not create another store call/goroutine.
	if got := s.Recent("saturated"); len(got) != 0 {
		t.Fatalf("saturated fallback = %v", got)
	}
	s.mu.Lock()
	if len(s.reads) != 1 || len(s.cache) != 1 {
		t.Fatal("read/cache concurrency limit exceeded")
	}
	s.mu.Unlock()
	close(release)
	waitAffinitySignal(t, done)
}
