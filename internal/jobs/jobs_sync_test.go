package jobs

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"
)

type memStore struct {
	mu      sync.Mutex
	rows    map[string]Job
	lastErr error
}

func newMemStore() *memStore {
	return &memStore{rows: map[string]Job{}}
}

func (s *memStore) Save(_ context.Context, j Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.rows[j.ID]; ok && old.CancelRequested {
		j.CancelRequested = true
	}
	s.rows[j.ID] = j
	return nil
}

func (s *memStore) Load(_ context.Context, limit int) ([]Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Job, 0, len(s.rows))
	for _, j := range s.rows {
		out = append(out, j)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Started > out[k].Started })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *memStore) RequestCancel(_ context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.rows[id]
	if !ok || j.State != Running {
		return false, nil
	}
	j.CancelRequested = true
	s.rows[id] = j
	return true, nil
}

func (s *memStore) get(id string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.rows[id]
	return j, ok
}

func waitStore(t *testing.T, s *memStore, id string, state State) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if j, ok := s.get(id); ok && j.State == state {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("store job %s never reached %s", id, state)
	return Job{}
}

func TestStoreMirrorsFinishedJobAcrossReplicas(t *testing.T) {
	store := newMemStore()
	a := &Manager{Workers: 1, Store: store, Owner: "replica-a"}
	j := a.Start("refresh", []string{"u1"}, func(context.Context, string) error { return nil })
	wait(t, a, j.ID)
	stored := waitStore(t, store, j.ID, Done)
	if stored.Owner != "replica-a" {
		t.Fatalf("owner = %q", stored.Owner)
	}

	b := &Manager{Store: store, Owner: "replica-b"}
	b.Sync(context.Background())
	got, ok := b.Get(j.ID)
	if !ok || got.State != Done || got.Done != 1 || got.Owner != "replica-a" {
		t.Fatalf("mirrored job = %+v ok=%v", got, ok)
	}
}

func TestSyncKeepsJobsNewestFirst(t *testing.T) {
	store := newMemStore()
	store.rows["old"] = Job{ID: "old", State: Done, Owner: "replica-a", Started: 100}
	store.rows["new"] = Job{ID: "new", State: Done, Owner: "replica-a", Started: 200}
	b := &Manager{Store: store, Owner: "replica-b"}
	b.Sync(context.Background())
	list := b.List()
	if len(list) != 2 || list[0].ID != "new" || list[1].ID != "old" {
		t.Fatalf("order = %+v", list)
	}
}

func TestRemoteCancelReachesOwner(t *testing.T) {
	store := newMemStore()
	a := &Manager{Workers: 1, Store: store, Owner: "replica-a"}
	started := make(chan struct{})
	j := a.Start("slow", []string{"u1"}, func(ctx context.Context, _ string) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	<-started
	waitStore(t, store, j.ID, Running)

	b := &Manager{Store: store, Owner: "replica-b"}
	b.Sync(context.Background())
	if !b.Cancel(j.ID) {
		t.Fatal("remote cancel was not accepted")
	}
	if stored, ok := store.get(j.ID); !ok || !stored.CancelRequested {
		t.Fatalf("cancel flag not stored: %+v ok=%v", stored, ok)
	}

	a.Sync(context.Background())
	done := wait(t, a, j.ID)
	if done.State != Cancelled {
		t.Fatalf("owner state = %s, want cancelled", done.State)
	}
}
