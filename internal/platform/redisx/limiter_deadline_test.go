package redisx

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
)

func TestLimiterReleaseBoundsStalledHeartbeat(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := Open(context.Background(), mr.Addr(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	l := NewLimiter(c)
	l.ttl = 600 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release, ok, err := l.Acquire(ctx, "acct", 1, "held")
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	defer release()
	started := make(chan struct{})
	var once sync.Once
	mr.Server().SetPreHook(func(_ *server.Peer, cmd string, _ ...string) bool {
		if cmd == "EVALSHA" || cmd == "EVAL" {
			once.Do(func() { close(started) })
			return true // accept renewal bytes, but never send a reply
		}
		return false // cleanup ZREM still works
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat never started")
	}
	cancel()
	start := time.Now()
	release()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("release exceeded renewal deadline: %v", elapsed)
	}
	if mr.Exists(l.prefix + "acct") {
		t.Fatal("release did not remove the slot")
	}
}

func TestLimiterQueueBudgetBoundsStalledRedis(t *testing.T) {
	for _, keySlot := range []bool{false, true} {
		t.Run(fmt.Sprintf("key=%v", keySlot), func(t *testing.T) {
			mr := miniredis.RunT(t)
			c, err := Open(context.Background(), mr.Addr(), "", 0)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			l := NewLimiter(c)
			mr.Server().SetPreHook(func(_ *server.Peer, cmd string, _ ...string) bool {
				return cmd == "EVALSHA" || cmd == "EVAL" // no script response
			})
			start := time.Now()
			var ok bool
			var release func()
			if keySlot {
				release, ok, err = l.AcquireKeyWait(context.Background(), "key", 1, "waiter", 80*time.Millisecond)
			} else {
				release, ok, err = l.AcquireWait(context.Background(), "acct", 1, "waiter", 80*time.Millisecond)
			}
			if release != nil {
				defer release()
			}
			if ok || err != nil {
				t.Errorf("queue budget expiry: ok=%v err=%v", ok, err)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("queue exceeded its I/O deadline: %v", elapsed)
			}
		})
	}
}

func TestLimiterQueuedLeaseOutlivesWaitContext(t *testing.T) {
	for _, keySlot := range []bool{false, true} {
		t.Run(fmt.Sprintf("key=%v", keySlot), func(t *testing.T) {
			mr := miniredis.RunT(t)
			c, err := Open(context.Background(), mr.Addr(), "", 0)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			l := NewLimiter(c)
			// Heartbeat/TTL tests need headroom for concurrent Windows CI load.
			// Observe a renewal directly; do not require a wall-clock TTL race.
			l.ttl = 6 * time.Second
			var release func()
			var ok bool
			key := l.prefix + "acct"
			if keySlot {
				key = "wb:keyinflight:key"
				release, ok, err = l.AcquireKeyWait(context.Background(), "key", 1, "held", 2*time.Second)
			} else {
				release, ok, err = l.AcquireWait(context.Background(), "acct", 1, "held", 2*time.Second)
			}
			if err != nil || !ok {
				t.Fatalf("acquire waiter: ok=%v err=%v", ok, err)
			}
			defer release()
			initial, err := c.ZScore(context.Background(), key, "held").Result()
			if err != nil {
				t.Fatal(err)
			}
			// The queue helper already canceled its operation context. A score
			// advance proves the returned lease still uses the request lifetime.
			deadline := time.Now().Add(2 * l.ttl)
			for {
				latest, err := c.ZScore(context.Background(), key, "held").Result()
				if err != nil {
					t.Fatal(err)
				}
				if latest > initial {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("returned lease stopped renewing: initial=%v latest=%v", initial, latest)
				}
				time.Sleep(20 * time.Millisecond)
			}
			var competitor func()
			if keySlot {
				competitor, ok, err = l.AcquireKey(context.Background(), "key", 1, "other")
			} else {
				competitor, ok, err = l.Acquire(context.Background(), "acct", 1, "other")
			}
			if competitor != nil {
				competitor()
			}
			if err != nil || ok {
				t.Fatalf("active lease expired after queue helper returned: ok=%v err=%v", ok, err)
			}
		})
	}
}
