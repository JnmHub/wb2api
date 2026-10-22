package pool

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"wb2api/internal/platform/redisx"
)

// Delay outside Redis models an old operation accepted by a transport/retry
// despite cancellation. Generation checking must happen when Redis commits.
type delayedVersionAffinityStore struct {
	*redisx.AffinityStore
	started chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	demotes int
}

func (s *delayedVersionAffinityStore) BindVersion(_ context.Context, key, uid, version string, ttl time.Duration, history int) (bool, error) {
	s.once.Do(func() { close(s.started); <-s.release })
	return s.AffinityStore.BindVersion(context.Background(), key, uid, version, ttl, history)
}

func (s *delayedVersionAffinityStore) Demote(ctx context.Context, key, uid string) error {
	s.mu.Lock()
	s.demotes++
	s.mu.Unlock()
	return s.AffinityStore.Demote(ctx, key, uid)
}

func redisAffinityForTest(t *testing.T) (*miniredis.Miniredis, *redisx.AffinityStore) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return mr, redisx.NewAffinityStore(client)
}

func TestAffinityDistributedFenceDelayedBindAfterDemote(t *testing.T) {
	for _, newer := range []bool{false, true} {
		for _, localDemote := range []bool{false, true} {
			t.Run(fmt.Sprintf("newer=%v/localDemote=%v", newer, localDemote), func(t *testing.T) {
				_, backend := redisAffinityForTest(t)
				delayed := &delayedVersionAffinityStore{AffinityStore: backend, started: make(chan struct{}), release: make(chan struct{})}
				a, b := NewSessionAffinity(), NewSessionAffinity()
				a.Timeout, b.Timeout = 5*time.Second, 5*time.Second
				a.SetStore(delayed)
				b.SetStore(backend)
				_, old := a.ObserveContext(context.Background(), "target")
				a.BindObserved("target", "account", old)
				waitAffinitySignal(t, delayed.started)
				if localDemote {
					// This marks the in-flight local write stale. Its eventual
					// completion must NOT blindly compensate with another Demote.
					a.Demote("target", "account")
				} else {
					b.Demote("target", "account")
				}
				if newer {
					_, current := b.ObserveContext(context.Background(), "target")
					b.BindObserved("target", "account", current)
					drainAffinity(t, b)
				}
				close(delayed.release)
				drainAffinity(t, a)
				got, err := backend.Recent(context.Background(), "target", 3)
				if err != nil {
					t.Fatal(err)
				}
				if newer && (len(got) != 1 || got[0] != "account") {
					t.Fatalf("late stale write deleted newer valid binding: %v", got)
				}
				if !newer && len(got) != 0 {
					t.Fatalf("late stale write resurrected binding: %v", got)
				}
				delayed.mu.Lock()
				defer delayed.mu.Unlock()
				want := 0
				if localDemote {
					want = 1
				}
				if delayed.demotes != want {
					t.Fatalf("unexpected blind compensation: demotes=%d, want %d", delayed.demotes, want)
				}
			})
		}
	}
}

func TestAffinityDistributedFenceQueuedBindUsesOriginalObservation(t *testing.T) {
	_, backend := redisAffinityForTest(t)
	delayed := &delayedVersionAffinityStore{AffinityStore: backend, started: make(chan struct{}), release: make(chan struct{})}
	a, b := NewSessionAffinity(), NewSessionAffinity()
	a.Timeout, b.Timeout = 5*time.Second, 5*time.Second
	a.SetStore(delayed)
	b.SetStore(backend)
	_, blocker := a.ObserveContext(context.Background(), "block")
	a.BindObserved("block", "other", blocker)
	waitAffinitySignal(t, delayed.started)
	_, old := a.ObserveContext(context.Background(), "target")
	a.BindObserved("target", "account", old)
	b.Demote("target", "account")
	_, fresh := b.ObserveContext(context.Background(), "target")
	b.BindObserved("target", "account", fresh)
	drainAffinity(t, b)
	close(delayed.release)
	drainAffinity(t, a)
	got, err := backend.Recent(context.Background(), "target", 3)
	if err != nil || len(got) != 1 || got[0] != "account" {
		t.Fatalf("queued stale write changed fresh binding: %v, %v", got, err)
	}
}

func TestAffinityObservationCannotBorrowNewGeneration(t *testing.T) {
	_, backend := redisAffinityForTest(t)
	a, b := NewSessionAffinity(), NewSessionAffinity()
	a.Timeout, b.Timeout = 5*time.Second, 5*time.Second
	a.SetStore(backend)
	b.SetStore(backend)
	_, old := a.ObserveContext(context.Background(), "target")
	b.Demote("target", "account")
	a.mu.Lock()
	a.cache["target"].loadedAt = time.Time{}
	a.mu.Unlock()
	_, fresh := a.ObserveContext(context.Background(), "target")
	// A later read on this same replica must not upgrade an older request.
	a.BindObserved("target", "account", old)
	drainAffinity(t, a)
	got, err := backend.Recent(context.Background(), "target", 3)
	if err != nil || len(got) != 0 {
		t.Fatalf("stale request borrowed fresh observation: %v, %v", got, err)
	}
	a.BindObserved("target", "account", fresh)
	drainAffinity(t, a)
	got, err = backend.Recent(context.Background(), "target", 3)
	if err != nil || len(got) != 1 {
		t.Fatalf("valid new observation did not bind: %v, %v", got, err)
	}
}

func TestAffinityObservationLocalDemoteAndEviction(t *testing.T) {
	for _, evict := range []bool{false, true} {
		t.Run(fmt.Sprint(evict), func(t *testing.T) {
			s := NewSessionAffinity()
			s.MaxEntries = 1
			_, old := s.ObserveContext(context.Background(), "target")
			if evict {
				s.Bind("other", "warm")
			} else {
				s.Demote("target", "account")
			}
			s.BindObserved("target", "account", old)
			if got := s.Recent("target"); len(got) != 0 {
				t.Fatalf("invalid local observation resurrected account: %v", got)
			}
			_, fresh := s.ObserveContext(context.Background(), "target")
			s.BindObserved("target", "account", fresh)
			if got := s.Recent("target"); len(got) != 1 {
				t.Fatalf("fresh local observation did not bind: %v", got)
			}
		})
	}
}

func TestAffinityVersionedBindDoesNotExtendObservationCacheAge(t *testing.T) {
	_, backend := redisAffinityForTest(t)
	s := NewSessionAffinity()
	s.Timeout = 5 * time.Second
	s.SetStore(backend)
	_, observation := s.ObserveContext(context.Background(), "target")
	s.mu.Lock()
	loadedAt := s.cache["target"].loadedAt
	s.mu.Unlock()
	s.BindObserved("target", "account", observation)
	drainAffinity(t, s)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.cache["target"].loadedAt.Equal(loadedAt) {
		t.Fatal("Bind extended distributed observation cache age")
	}
}

func TestAffinityRecentLocalMissDoesNotEvictWarmBinding(t *testing.T) {
	s := NewSessionAffinity()
	s.MaxEntries = 1
	s.Bind("warm", "account")
	if got := s.Recent("absent"); len(got) != 0 {
		t.Fatalf("absent=%v", got)
	}
	if got := s.Recent("warm"); len(got) != 1 {
		t.Fatalf("ordinary local miss evicted warm binding: %v", got)
	}
}

func TestAffinityVersionedBindWithoutReadStaysLocal(t *testing.T) {
	_, backend := redisAffinityForTest(t)
	s := NewSessionAffinity()
	s.SetStore(backend)
	s.Bind("target", "account")
	drainAffinity(t, s)
	s.mu.Lock()
	gotLocal := affinityUIDs(s.cache["target"].entries)
	s.mu.Unlock()
	if len(gotLocal) != 1 {
		t.Fatalf("local fallback missing: %v", gotLocal)
	}
	if got, err := backend.Recent(context.Background(), "target", 3); err != nil || len(got) != 0 {
		t.Fatalf("Bind manufactured an observation: %v, %v", got, err)
	}
}
