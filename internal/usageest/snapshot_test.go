package usageest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/usagedb"
)

type observedWaitContext struct {
	context.Context
	waiting chan<- struct{}
}

func (c observedWaitContext) Done() <-chan struct{} { c.waiting <- struct{}{}; return nil }

func TestScheduleSnapshotNeverWaitsForSlowRefresh(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	est := &Estimator{RefreshTimeout: time.Second, pointQuery: func(ctx context.Context, from, to time.Time) ([]usagedb.PairPoint, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	done := make(chan error, 1)
	go func() { done <- est.Refresh(context.Background()) }()
	<-entered
	var wg sync.WaitGroup
	waiting := make(chan struct{}, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = est.Refresh(observedWaitContext{Context: context.Background(), waiting: waiting})
		}()
	}
	// Wait until every concurrent reader has joined the held flight.
	for i := 0; i < 16; i++ {
		<-waiting
	}
	// Direct calls complete while backend is held. Cold priority is safely off.
	for i := 0; i < 1000; i++ {
		if s := est.ScheduleSnapshot(); s != nil {
			t.Fatal("cold priority must be off")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("refresh not coalesced")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent refresh made %d backend calls", calls.Load())
	}
	if est.ScheduleSnapshot() == nil {
		t.Fatal("no completed publication")
	}
}

func TestWarmDispatchDoesNotWaitForBlockedQuery(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	est := &Estimator{Pool: testPool(t, "u1", "u2"), RefreshTimeout: time.Second}
	if err := est.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	warm := pool.NewRemainingSnapshot(42, []pool.RemainingWeight{{Realm: "intl", Model: "m", UID: "u1", Weight: 4}})
	est.schedule.Store(warm)
	est.pointQuery = func(ctx context.Context, _, _ time.Time) ([]usagedb.PairPoint, error) {
		close(entered)
		select {
		case <-release:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	done := make(chan error, 1)
	go func() { done <- est.Refresh(context.Background()) }()
	<-entered
	for i := 0; i < 1000; i++ {
		est.Pool.ApplyRemainingSnapshot(est.ScheduleSnapshot())
		if a := est.Pool.Pick("intl", "m", nil); a == nil || a.UID != "u1" {
			t.Fatal("warm dispatch blocked or lost priority")
		}
	}
	if n := testing.AllocsPerRun(1000, func() { est.Pool.ApplyRemainingSnapshot(est.ScheduleSnapshot()); est.Pool.Pick("intl", "m", nil) }); n != 0 {
		t.Fatalf("allocs=%v", n)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRefreshInvalidationRejectsOldPublication(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	est := &Estimator{pointQuery: func(context.Context, time.Time, time.Time) ([]usagedb.PairPoint, error) {
		close(entered)
		<-release
		return nil, nil
	}}
	done := make(chan error, 1)
	go func() { done <- est.Refresh(context.Background()) }()
	<-entered
	est.Invalidate()
	close(release)
	if err := <-done; !errors.Is(err, errInvalidated) {
		t.Fatalf("err=%v", err)
	}
	if est.ScheduleSnapshot() != nil || est.cached != nil {
		t.Fatal("stale build published")
	}
	est.pointQuery = nil
	if err := est.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if est.ScheduleSnapshot().Revision() != 1 {
		t.Fatal("invalid build advanced revision")
	}
}

func TestRefreshTimeoutRetainsAccuratePayload(t *testing.T) {
	now := time.Now()
	est := &Estimator{Now: func() time.Time { return now }, TTL: time.Nanosecond, RefreshTimeout: 20 * time.Millisecond}
	if err := est.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := est.ScheduleSnapshot()
	cached := est.cached
	now = now.Add(time.Second)
	est.pointQuery = func(ctx context.Context, _, _ time.Time) ([]usagedb.PairPoint, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err := est.Refresh(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if est.ScheduleSnapshot() != before || est.cached != cached {
		t.Fatal("failed read destroyed accurate data")
	}
}

func TestStartCancellationCancelsBackend(t *testing.T) {
	entered, stopped := make(chan struct{}), make(chan struct{})
	est := &Estimator{pointQuery: func(ctx context.Context, _, _ time.Time) ([]usagedb.PairPoint, error) {
		close(entered)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	est.Start(ctx)
	<-entered
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("lifecycle cancellation not propagated")
	}
}

func TestLimitEventPhysicalRetentionPreservesLegacyNewestSamples(t *testing.T) {
	est := &Estimator{Dir: t.TempDir()}
	var log strings.Builder
	now := time.Now()
	for i := 0; i < maxEvents+200; i++ {
		fmt.Fprintf(&log, "{\"at\":%d,\"account\":\"u%d\",\"model\":\"m\",\"reset\":%d}\n", now.Unix(), i, now.Unix())
	}
	if err := os.WriteFile(est.eventsPath(), []byte(log.String()), 0644); err != nil {
		t.Fatal(err)
	}
	est.initEvents()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := est.FlushEvents(ctx); err != nil {
		t.Fatal(err)
	}
	events := est.loadEvents()
	if len(events) != maxEvents || events[0].Account != "u200" {
		t.Fatalf("retained %d, first=%+v", len(events), events[0])
	}
	for i := 0; i < 20; i++ {
		est.NoteLimit(fmt.Sprintf("new%d", i), "m", "intl", now.Add(time.Hour), now)
	}
	if err := est.FlushEvents(ctx); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(est.eventsPath())
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(raw), "\n"); lines != maxEvents {
		t.Fatalf("physical log has %d lines", lines)
	}
	reload := &Estimator{Dir: est.Dir}
	got := reload.loadEvents()
	if len(got) != maxEvents || got[0].Account != "u220" || got[len(got)-1].Account != "new19" {
		t.Fatalf("retained boundary mismatch: %d %v %v", len(got), got[0], got[len(got)-1])
	}
	if err := reload.FlushEvents(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLimitEventReadFailureDoesNotDestroyExistingLog(t *testing.T) {
	est := &Estimator{Dir: t.TempDir()}
	// A scanner failure must never compact the unread suffix away.
	raw := strings.Repeat("x", (1<<20)+1) + "\n{\"at\":1,\"account\":\"legacy\",\"model\":\"m\"}\n"
	if err := os.WriteFile(est.eventsPath(), []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	est.NoteLimit("new", "m", "intl", now.Add(time.Hour), now)
	if err := est.FlushEvents(context.Background()); err == nil {
		t.Fatal("partial read not reported")
	}
	got, err := os.ReadFile(est.eventsPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != raw {
		t.Fatal("unread retained estimates destroyed")
	}
}

func TestScheduleSnapshotZeroAllocations(t *testing.T) {
	est := &Estimator{}
	if err := est.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := testing.AllocsPerRun(1000, func() { _ = est.ScheduleSnapshot().Weight("intl", "m", "u") }); n != 0 {
		t.Fatalf("allocs=%v", n)
	}
}

var benchmarkRemainingSink map[string]map[string]int

// BenchmarkRemainingRequest compares the compatibility map-copy API formerly
// used on every request with the immutable dispatch API (same warm estimate).
func BenchmarkRemainingRequest(b *testing.B) {
	for _, n := range []int{100, 1000} {
		now := time.Now()
		est := &Estimator{Now: func() time.Time { return now }, TTL: time.Hour, cached: &Payload{}}
		est.builtAt = now
		var weights []pool.RemainingWeight
		budget, remaining := int64(100), int64(3)
		for i := 0; i < n; i++ {
			uid := fmt.Sprintf("u%d", i)
			est.cached.Rows = append(est.cached.Rows, Row{UID: uid, Realm: "intl", Model: "m", Budget: &budget, Remaining: &remaining})
			weights = append(weights, pool.RemainingWeight{Realm: "intl", Model: "m", UID: uid, Weight: 4})
		}
		est.schedule.Store(pool.NewRemainingSnapshot(1, weights))
		p := pool.New(nil)
		b.Run(fmt.Sprintf("legacy-cached-%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				// Reproduce the old ApplyRemainingWeights map copy (without the
				// new compatibility adapter's snapshot construction).
				w := est.ScheduleWeights()
				cp := make(map[string]map[string]int, len(w))
				for model, byUID := range w {
					inner := make(map[string]int, len(byUID))
					for uid, weight := range byUID {
						if weight > 1 {
							inner[uid] = weight
						}
					}
					if len(inner) > 0 {
						cp[model] = inner
					}
				}
				benchmarkRemainingSink = cp
			}
		})
		b.Run(fmt.Sprintf("snapshot-%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				p.ApplyRemainingSnapshot(est.ScheduleSnapshot())
			}
		})
	}
}

func BenchmarkScheduleSnapshot(b *testing.B) {
	est := &Estimator{}
	if err := est.Refresh(context.Background()); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = est.ScheduleSnapshot().Weight("intl", "m", "u")
	}
}
