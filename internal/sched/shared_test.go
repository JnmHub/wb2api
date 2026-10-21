package sched

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeShared struct {
	mu        sync.Mutex
	remote    map[string]SharedSample
	published map[string]SharedSample
	exclude   string
}

func (f *fakeShared) Publish(_ context.Context, _ string, samples map[string]SharedSample) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = make(map[string]SharedSample, len(samples))
	for uid, st := range samples {
		f.published[uid] = st
	}
	return nil
}

func (f *fakeShared) Load(_ context.Context, exclude string) (map[string]SharedSample, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exclude = exclude
	out := make(map[string]SharedSample, len(f.remote))
	for uid, st := range f.remote {
		out[uid] = st
	}
	return out, nil
}

func TestSharedStatsUsesWorstPeerSample(t *testing.T) {
	now := time.Now()
	fake := &fakeShared{remote: map[string]SharedSample{
		"a": {TTFT: 20000, Err: 0.9, Seen: now, HasTTFT: true, Samples: 4},
	}}
	s := NewStats(15*time.Second, 0.5)
	s.SetShared(fake, "replica-a")
	s.Observe("a", time.Second, false)
	s.SyncShared(context.Background())

	if !s.ShouldEscape("a") {
		t.Fatal("peer sample should make the account escape")
	}
	ttft, errRate, ok := s.Score("a")
	if !ok || ttft != 20000 || errRate != 0.9 {
		t.Fatalf("score = %v %v %v, want peer worst values", ttft, errRate, ok)
	}
	if fake.exclude != "replica-a" {
		t.Fatalf("exclude = %q", fake.exclude)
	}
}

func TestSharedStatsPublishesLocalSnapshot(t *testing.T) {
	fake := &fakeShared{}
	s := NewStats(15*time.Second, 0.5)
	s.SetShared(fake, "replica-b")
	s.Observe("b", 123*time.Millisecond, false)
	s.SyncShared(context.Background())

	st, ok := fake.published["b"]
	if !ok {
		t.Fatal("local sample was not published")
	}
	if st.TTFT != 123 || !st.HasTTFT || st.Seen.IsZero() {
		t.Fatalf("published = %+v", st)
	}
}

func TestSharedStatsIgnoresStalePeerSample(t *testing.T) {
	fake := &fakeShared{remote: map[string]SharedSample{
		"c": {TTFT: 60000, Err: 1, Seen: time.Now().Add(-10 * time.Minute), HasTTFT: true},
	}}
	s := NewStats(15*time.Second, 0.5)
	s.SetShared(fake, "replica-c")
	s.Observe("c", time.Second, false)
	s.SyncShared(context.Background())

	if s.ShouldEscape("c") {
		t.Fatal("stale peer sample must not pin an account")
	}
}
