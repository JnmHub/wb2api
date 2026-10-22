package redisx

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type occupancyHook struct {
	batches  atomic.Int32
	commands atomic.Int32
	block    chan struct{}
	started  chan struct{}
	once     sync.Once
	delay    time.Duration
}

func (*occupancyHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) { return next(ctx, network, addr) }
}
func (*occupancyHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }
func (h *occupancyHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		h.batches.Add(1)
		h.commands.Add(int32(len(cmds)))
		h.once.Do(func() {
			if h.started != nil {
				close(h.started)
			}
		})
		if h.block != nil {
			select {
			case <-h.block:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if h.delay > 0 {
			t := time.NewTimer(h.delay)
			defer t.Stop()
			select {
			case <-t.C:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return next(ctx, cmds)
	}
}
func capacityLimiter(t *testing.T) (*Limiter, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := Open(context.Background(), mr.Addr(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return NewLimiter(c), mr
}
func TestOccupancyTwoMillisecondIOBoundedBatches(t *testing.T) {
	for _, n := range []int{100, 1000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			l, _ := capacityLimiter(t)
			hook := &occupancyHook{delay: 2 * time.Millisecond}
			l.c.AddHook(hook)
			ids := make([]string, n)
			for i := range ids {
				ids[i] = fmt.Sprint(i)
			}
			started := time.Now()
			values, err := l.Occupancy(context.Background(), ids, false)
			if err != nil || len(values) != n || hook.batches.Load() != int32((n+occupancyBatch-1)/occupancyBatch) {
				t.Fatalf("n=%d batches=%d values=%d err=%v", n, hook.batches.Load(), len(values), err)
			}
			t.Logf("n=%d batches=%d elapsed=%s (2ms simulated network perbatch)", n, hook.batches.Load(), time.Since(started))
		})
	}
}

func TestOccupancyLiveCountsDoNotMutateOwners(t *testing.T) {
	l, mr := capacityLimiter(t)
	now := time.Now().UnixMilli()
	for member, score := range map[string]int64{"expired": now - l.ttl.Milliseconds() - 1, "live": now, "future": now + 1000} {
		mr.ZAdd(l.prefix+"a", float64(score), member)
	}
	got, err := l.Occupancy(context.Background(), []string{"a", "missing"}, false)
	if err != nil || got["a"] != 2 || got["missing"] != 0 {
		t.Fatalf("occupancy=%v err=%v", got, err)
	}
	if n, _ := l.c.ZCard(context.Background(), l.prefix+"a").Result(); n != 3 {
		t.Fatal("hint removed owner members")
	}
	if mr.Exists(l.prefix + "missing") {
		t.Fatal("hint created key")
	}
}
func TestOccupancyBatchCacheAndFresh(t *testing.T) {
	l, mr := capacityLimiter(t)
	hook := &occupancyHook{}
	l.c.AddHook(hook)
	ids := make([]string, 1000)
	for i := range ids {
		ids[i] = fmt.Sprint(i)
	}
	got, err := l.Occupancy(context.Background(), ids, false)
	if err != nil || len(got) != len(ids) {
		t.Fatalf("read %d %v", len(got), err)
	}
	if hook.batches.Load() != 2 || hook.commands.Load() != 1000 {
		t.Fatalf("not pipelined: %d batches %d commands", hook.batches.Load(), hook.commands.Load())
	}
	mr.ZAdd(l.prefix+ids[0], float64(time.Now().UnixMilli()), "owner")
	got, err = l.Occupancy(context.Background(), ids, false)
	if err != nil || got[ids[0]] != 0 || hook.batches.Load() != 2 {
		t.Fatal("cache miss")
	}
	got, err = l.Occupancy(context.Background(), ids, true)
	if err != nil || got[ids[0]] != 1 || hook.batches.Load() != 4 {
		t.Fatalf("fresh did not bypass stale cache: %v %v", got, err)
	}
}
func TestOccupancySingleflightCanceledCallerDoesNotPoisonPeers(t *testing.T) {
	l, _ := capacityLimiter(t)
	hook := &occupancyHook{block: make(chan struct{}), started: make(chan struct{})}
	l.c.AddHook(hook)
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := l.Occupancy(ctx, []string{"a"}, false); first <- err }()
	<-hook.started
	second := make(chan error, 1)
	go func() { _, err := l.Occupancy(context.Background(), []string{"a"}, false); second <- err }()
	cancel()
	if err := <-first; err != context.Canceled {
		t.Fatalf("cancel=%v", err)
	}
	close(hook.block)
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if hook.batches.Load() != 1 {
		t.Fatalf("duplicate refresh: %d", hook.batches.Load())
	}
}
func TestOccupancyConcurrentSnapshotsAndIndependentResults(t *testing.T) {
	l, _ := capacityLimiter(t)
	hook := &occupancyHook{block: make(chan struct{}), started: make(chan struct{})}
	l.c.AddHook(hook)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := l.Occupancy(context.Background(), []string{"a", "b"}, false)
			if err == nil {
				got["a"] = 100
			}
			errs <- err
		}()
	}
	<-hook.started
	close(hook.block)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := l.Occupancy(context.Background(), []string{"a"}, false)
	if err != nil || got["a"] != 0 {
		t.Fatalf("result aliases cache: %v %v", got, err)
	}
	if hook.batches.Load() != 1 {
		t.Fatalf("duplicate refreshes: %d", hook.batches.Load())
	}
}
func TestAcquireWithinUsesRequestLeaseContext(t *testing.T) {
	l, _ := capacityLimiter(t)
	rel, ok, err := l.AcquireWithin(context.Background(), "a", 1, "owner", time.Second)
	if err != nil || !ok {
		t.Fatalf("bounded probe: %v %v", ok, err)
	}
	defer rel()
	if other, ok, err := l.AcquireWithin(context.Background(), "a", 1, "other", time.Second); err != nil || ok {
		if other != nil {
			other()
		}
		t.Fatal("atomic cap bypass")
	}
	rel()
	rel()
	other, ok, err := l.AcquireWithin(context.Background(), "a", 1, "other", time.Second)
	if err != nil || !ok {
		t.Fatal("release wedged")
	}
	other()
}
