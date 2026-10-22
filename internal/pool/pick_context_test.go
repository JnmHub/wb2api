package pool

import (
	"context"
	"testing"
	"time"
)

func TestPickForSessionContextCancelsSharedMiss(t *testing.T) {
	p := New(mkRecs(1, time.Hour))
	started := make(chan struct{})
	store := &blockingAffinityStore{fakeAffinityStore: newFakeAffinityStore()}
	store.recentHook = func(ctx context.Context, _ string) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	p.Affinity().SetStore(store)
	p.Affinity().Timeout = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *Account, 1)
	go func() { done <- p.PickForSessionBestContext(ctx, "intl", "cancel", nil, "m", nil, nil) }()
	waitAffinitySignal(t, started)
	cancel()
	select {
	case a := <-done:
		if a != nil {
			t.Fatalf("cancelled request selected %s", a.UID)
		}
	case <-time.After(time.Second):
		t.Fatal("selection did not follow request cancellation")
	}
	drainAffinity(t, p.Affinity())
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.bindCalls != 0 {
		t.Fatalf("cancelled selection published %d binds", store.bindCalls)
	}
}

func TestPickForSessionContextPreCancelled(t *testing.T) {
	p := New(mkRecs(1, time.Hour))
	store := newFakeAffinityStore()
	p.Affinity().SetStore(store)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, session := range []string{"", "session"} {
		if a := p.PickForSessionBestContext(ctx, "intl", session, nil, "m", nil, nil); a != nil {
			t.Fatalf("pre-cancelled request selected %s", a.UID)
		}
	}
	if store.recentCalls != 0 {
		t.Fatalf("pre-cancelled selection made %d shared reads", store.recentCalls)
	}
}
