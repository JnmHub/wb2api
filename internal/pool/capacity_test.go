package pool

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTryAcquireReservesCapacityAtomically(t *testing.T) {
	a := NewAccount(mkRecs(1, time.Hour)[0])
	a.SetMaxInflight(3)
	var wg sync.WaitGroup
	var admitted atomic.Int32
	var successfulMu sync.Mutex
	var successfulReleases []func()
	start := make(chan struct{})
	for i := 0; i < 128; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if release, ok := a.TryAcquire(0); ok {
				admitted.Add(1)
				successfulMu.Lock()
				successfulReleases = append(successfulReleases, release)
				successfulMu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := admitted.Load(); got != 3 || a.Inflight() != 3 {
		t.Fatalf("admitted=%d inflight=%d, want 3 each", got, a.Inflight())
	}
	if !a.AtCapacity(0) || a.Ready("", time.Now()) {
		t.Fatal("account at its local cap must be reported full and not ready")
	}
	if !a.HealthReady("", time.Now()) {
		t.Fatal("capacity must not hide account health")
	}
	if len(successfulReleases) == 0 {
		t.Fatal("successful reservations must return release functions")
	}
	successfulReleases[0]()
	if release, ok := a.TryAcquire(0); !ok {
		t.Fatal("exactly one released slot must be reusable")
	} else {
		release()
	}
	for _, release := range successfulReleases[1:] {
		release()
	}
}

func TestTryAcquireUnlimited(t *testing.T) {
	a := NewAccount(mkRecs(1, time.Hour)[0])
	var releases []func()
	for i := 0; i < 20; i++ {
		release, ok := a.TryAcquire(0)
		if !ok {
			t.Fatal("zero capacity means unlimited")
		}
		releases = append(releases, release)
	}
	for _, release := range releases {
		release()
	}
	if a.Inflight() != 0 {
		t.Fatal("reservation leaked")
	}
}
