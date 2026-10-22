package settings

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRefreshConcurrentViewRevCoalescesLoad(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(map[bool]string{false: "cold", true: "warm"}[warm], func(t *testing.T) {
			var loads atomic.Int32
			s := OpenWith(func() (Doc, error) {
				loads.Add(1)
				time.Sleep(120 * time.Millisecond)
				return Doc{"version": "loaded", "api_keys": []any{map[string]any{"id": "key", "key": "secret", "enabled": true}}}, nil
			}, nil)
			if warm {
				s.snap.Store(&snapshot{rev: 1, exists: true, doc: Doc{"version": "old"}})
			}
			const readers = 40
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := 0; i < readers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					if i%2 == 0 {
						if got := s.View()["version"]; got != "loaded" && !(warm && got == "old") {
							t.Errorf("cold reader saw incomplete document: %v", got)
						}
					} else if rev := s.Rev(); rev == 0 {
						t.Error("cold reader saw uninitialized revision")
					}
				}(i)
			}
			close(start)
			wg.Wait()
			if got := loads.Load(); got != 1 {
				t.Fatalf("concurrent cold load count = %d, want 1", got)
			}
			if keys := s.APIKeys(false); len(keys) != 1 || keys[0].Key != "secret" {
				t.Fatalf("coalescing lost authentication configuration: %v", keys)
			}
		})
	}
}

func TestRefreshSlowLoadNeverOverlapsAcrossIntervals(t *testing.T) {
	var loads, active, maximum atomic.Int32
	started, resume := make(chan struct{}), make(chan struct{})
	var release sync.Once
	unblock := func() { release.Do(func() { close(resume) }) }
	defer unblock()
	s := OpenWith(func() (Doc, error) {
		loads.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old; old = maximum.Load() {
			if maximum.CompareAndSwap(old, n) {
				break
			}
		}
		if loads.Load() == 1 {
			close(started)
			<-resume
		}
		return Doc{"version": "fresh"}, nil
	}, nil)
	s.snap.Store(&snapshot{rev: 1, exists: true, doc: Doc{"version": "old"}})
	first := make(chan Doc, 1)
	go func() { first <- s.View() }()
	<-started
	// Span multiple polling opportunities while the first read is in flight.
	time.Sleep(CheckInterval + 40*time.Millisecond)
	const readers = 20
	var entered, done sync.WaitGroup
	for i := 0; i < readers; i++ {
		entered.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			entered.Done()
			if got := s.View()["version"]; got != "fresh" {
				t.Errorf("due reader did not join refresh: %v", got)
			}
		}()
	}
	entered.Wait()
	time.Sleep(30 * time.Millisecond)
	if got := loads.Load(); got != 1 || maximum.Load() != 1 {
		t.Errorf("overlapping slow reads: calls=%d maximum=%d", got, maximum.Load())
	}
	unblock()
	if got := <-first; got["version"] != "fresh" {
		t.Fatalf("initiating reader = %v", got)
	}
	done.Wait()
	if got := loads.Load(); got != 1 {
		t.Fatalf("waiters repeated completed slow flight: loads=%d", got)
	}
	// A genuinely later call is due immediately: completion must not start a
	// new interval and extend the authorization freshness window.
	_ = s.View()
	if got := loads.Load(); got != 2 || maximum.Load() != 1 {
		t.Fatalf("later due read = loads %d maximum %d", got, maximum.Load())
	}
}

func TestRefreshWarmFlightCannotRestoreRevokedKey(t *testing.T) {
	for _, failLoad := range []bool{false, true} {
		t.Run(map[bool]string{false: "stale result", true: "load error"}[failLoad], func(t *testing.T) {
			started, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(resume) }) }
			defer unblock()
			old := func() Doc {
				return Doc{"api_keys": []any{map[string]any{"id": "key", "key": "secret", "enabled": true}}}
			}
			s := OpenWithUpdater(func() (Doc, error) {
				close(started)
				<-resume
				if failLoad {
					return nil, errors.New("backend unavailable")
				}
				return old(), nil
			}, nil, func(fn func(Doc) error) (Doc, error) {
				d := old()
				if err := fn(d); err != nil {
					return nil, err
				}
				return d, nil
			})
			s.snap.Store(&snapshot{rev: 1, exists: true, doc: old()})
			done := make(chan Doc, 1)
			go func() { done <- s.View() }()
			<-started
			committed := make(chan error, 1)
			go func() { committed <- s.DeleteKey("key") }()
			select {
			case err := <-committed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				unblock()
				<-done
				<-committed
				t.Fatal("refresh I/O blocked committed key revocation")
			}
			unblock()
			got := <-done
			row := got["api_keys"].([]any)[0].(map[string]any)
			if row["key"] != "" || row["enabled"] != false || row["deleted_at"] == "" {
				t.Fatalf("in-flight refresh returned pre-revocation document: %v", row)
			}
			if keys := s.APIKeys(false); len(keys) != 0 {
				t.Fatalf("refresh restored revoked key: %v", keys)
			}
		})
	}
}

func TestRefreshLegacyUpdateDoesNotReuseOlderFlight(t *testing.T) {
	started, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	defer unblock()
	var reads atomic.Int32
	var saved Doc
	s := OpenWith(func() (Doc, error) {
		if reads.Add(1) == 1 {
			close(started)
			<-resume
			return Doc{"sibling": "old"}, nil
		}
		return Doc{"sibling": "fresh", "unknown_future": true}, nil
	}, func(d Doc) error { saved = d; return nil })
	s.snap.Store(&snapshot{exists: true, doc: Doc{"sibling": "cached"}})
	reader := make(chan Doc, 1)
	go func() { reader <- s.View() }()
	<-started
	updated := make(chan error, 1)
	go func() {
		updated <- s.Update(func(d Doc) error { d["local"] = true; return nil })
	}()
	// Legacy Update invalidates before its forced read. Wait for that exact
	// transition to ensure it queues behind the older in-flight load.
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for s.snap.Load() != nil {
		select {
		case <-deadline.C:
			unblock()
			<-reader
			<-updated
			t.Fatal("legacy Update did not invalidate its mutation read")
		case <-tick.C:
		}
	}
	unblock()
	<-reader
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	if reads.Load() != 2 || saved["sibling"] != "fresh" || saved["unknown_future"] != true || saved["local"] != true {
		t.Fatalf("mutation reused older flight and lost external fields: reads=%d saved=%v", reads.Load(), saved)
	}
}

func TestRefreshErrorFlightReleasesAndRetries(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(map[bool]string{false: "cold", true: "warm"}[warm], func(t *testing.T) {
			var reads atomic.Int32
			s := OpenWith(func() (Doc, error) {
				if reads.Add(1) == 1 {
					return nil, errors.New("temporary outage")
				}
				return Doc{"recovered": true}, nil
			}, nil)
			if warm {
				s.snap.Store(&snapshot{exists: true, doc: Doc{"last_good": true}})
			}
			got := s.View()
			if warm && got["last_good"] != true {
				t.Fatalf("error discarded last good document: %v", got)
			}
			if !warm && len(got) != 0 {
				t.Fatalf("cold error behavior changed: %v", got)
			}
			_ = s.View()
			if reads.Load() != 1 {
				t.Fatal("error bypassed retry interval")
			}
			s.lastCheck.Store(0)
			if got := s.View(); got["recovered"] != true || reads.Load() != 2 {
				t.Fatalf("failed refresh kept flight locked: got=%v reads=%d", got, reads.Load())
			}
		})
	}
}
