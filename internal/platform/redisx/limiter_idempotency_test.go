package redisx

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/redis/go-redis/v9"
)

func TestLimiterCommittedAcquireRetry(t *testing.T) {
	for _, namespace := range []string{"account", "key"} {
		t.Run(namespace, func(t *testing.T) {
			mr := miniredis.RunT(t)
			ctx := context.Background()
			c, err := Open(ctx, mr.Addr(), "", 0)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			l := NewLimiter(c)
			acquire, key := l.Acquire, l.prefix+"id"
			if namespace == "key" {
				acquire, key = l.AcquireKey, "wb:keyinflight:id"
			}
			member := "same-logical-acquisition"
			// Commit without creating a heartbeat/release, as if the response
			// never arrived. Shorten the key TTL independently of the score.
			old := time.Now().Add(-time.Minute).UnixMilli()
			n, err := acquireScript.Run(ctx, c.Client, []string{key}, old, l.ttl.Milliseconds(), 1, member).Int()
			if err != nil || n != 1 {
				t.Fatalf("seed commit: n=%d err=%v", n, err)
			}
			mr.SetTTL(key, time.Second)
			release, ok, err := acquire(ctx, "id", 1, member)
			if release != nil {
				defer release()
			}
			if err != nil || !ok || release == nil {
				t.Fatalf("recover committed lease: ok=%v release=%v err=%v", ok, release != nil, err)
			}
			if count := c.ZCard(ctx, key).Val(); count != 1 {
				t.Fatalf("retry changed occupancy: %d", count)
			}
			if score := c.ZScore(ctx, key, member).Val(); score <= float64(old) {
				t.Fatalf("retry did not refresh score: %v <= %d", score, old)
			}
			if ttl := mr.TTL(key); ttl != l.ttl {
				t.Fatalf("retry did not refresh TTL: %v", ttl)
			}
			other, ok, err := acquire(ctx, "id", 1, "distinct-request")
			if other != nil {
				other()
			}
			if err != nil || ok || other != nil {
				t.Fatalf("distinct request exceeded limit: ok=%v err=%v", ok, err)
			}
			release()
			if count := c.ZCard(ctx, key).Val(); count != 0 {
				t.Fatalf("recovered release retained %d members", count)
			}
			other, ok, err = acquire(ctx, "id", 1, "distinct-request")
			if other != nil {
				defer other()
			}
			if err != nil || !ok {
				t.Fatalf("released capacity unavailable: ok=%v err=%v", ok, err)
			}
		})
	}
}

func TestLimiterDistinctConcurrentMembersStayBounded(t *testing.T) {
	for _, namespace := range []string{"account", "key"} {
		t.Run(namespace, func(t *testing.T) {
			mr := miniredis.RunT(t)
			ctx := context.Background()
			c, err := Open(ctx, mr.Addr(), "", 0)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			l := NewLimiter(c)
			acquire, key := l.Acquire, l.prefix+"id"
			if namespace == "key" {
				acquire, key = l.AcquireKey, "wb:keyinflight:id"
			}
			type result struct {
				release func()
				ok      bool
				err     error
			}
			const contenders, limit = 16, 3
			start := make(chan struct{})
			results := make(chan result, contenders)
			for i := 0; i < contenders; i++ {
				go func(i int) {
					<-start
					release, ok, err := acquire(ctx, "id", limit, fmt.Sprintf("member-%d", i))
					results <- result{release, ok, err}
				}(i)
			}
			close(start)
			admitted := 0
			for i := 0; i < contenders; i++ {
				r := <-results
				if r.release != nil {
					defer r.release() // keep all winning slots until every result arrives
				}
				if r.err != nil {
					t.Errorf("acquire: %v", r.err)
				}
				if r.ok {
					admitted++
				}
			}
			if count := c.ZCard(ctx, key).Val(); admitted != limit || count != limit {
				t.Fatalf("concurrent admission: admitted=%d occupancy=%d limit=%d", admitted, count, limit)
			}
		})
	}
}

func TestLimiterAcquireScriptExpiryAndLoweredLimit(t *testing.T) {
	for _, key := range []string{"wb:inflight:acct", "wb:keyinflight:key"} {
		t.Run(key, func(t *testing.T) {
			mr := miniredis.RunT(t)
			ctx := context.Background()
			c, err := Open(ctx, mr.Addr(), "", 0)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			const now, ttl int64 = 100000, 10000
			run := func(at int64, limit int, member string, want int) {
				t.Helper()
				mr.SetTime(time.UnixMilli(at))
				n, err := acquireScript.Run(ctx, c.Client, []string{key}, at, ttl, limit, member).Int()
				if err != nil || n != want {
					t.Fatalf("acquire(%d, %d, %q): n=%d want=%d err=%v", at, limit, member, n, want, err)
				}
			}
			run(now, 2, "held", 1)
			run(now, 2, "other", 1)
			// Lowering a limit does not evict either existing owner, but must
			// reject new members until occupancy falls below the new limit.
			mr.SetTTL(key, time.Second)
			run(now+1, 1, "held", 1)
			if score := c.ZScore(ctx, key, "held").Val(); score != float64(now+1) {
				t.Fatalf("retry score=%v", score)
			}
			if got := mr.TTL(key); got != time.Duration(ttl)*time.Millisecond {
				t.Fatalf("retry TTL=%v", got)
			}
			run(now+1, 1, "third", 0)
			if count := c.ZCard(ctx, key).Val(); count != 2 {
				t.Fatalf("lowered-limit retry changed occupancy: %d", count)
			}
			// At the inclusive expiry boundary, held is no longer an owner.
			// Keep other fresh: an expired same-member retry cannot bypass it.
			if err := c.ZAdd(ctx, key, redis.Z{Score: float64(now + ttl), Member: "other"}).Err(); err != nil {
				t.Fatal(err)
			}
			run(now+ttl+1, 1, "held", 0)
			if members, err := c.ZRange(ctx, key, 0, -1).Result(); err != nil || len(members) != 1 || members[0] != "other" {
				t.Fatalf("expired owner retained: members=%v err=%v", members, err)
			}
			// Once all owners have expired, normal admission is allowed again.
			run(now+2*ttl, 1, "held", 1)
			run(now+2*ttl, 1, "third", 0)
		})
	}
}

func TestLimiterAcquireRetriesLostCommitReply(t *testing.T) {
	for _, namespace := range []string{"account", "key"} {
		t.Run(namespace, func(t *testing.T) {
			mr := miniredis.RunT(t)
			ctx := context.Background()
			c, err := Open(ctx, mr.Addr(), "", 0)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			l := NewLimiter(c)
			acquire, key := l.Acquire, l.prefix+"id"
			if namespace == "key" {
				acquire, key = l.AcquireKey, "wb:keyinflight:id"
			}
			if err := acquireScript.Load(ctx, c.Client).Err(); err != nil {
				t.Fatal(err)
			}
			var injected atomic.Bool
			committed := make(chan string, 1)
			mr.Server().SetPreHook(func(peer *server.Peer, cmd string, args ...string) bool {
				if cmd != "EVALSHA" || !injected.CompareAndSwap(false, true) {
					return false
				}
				// Execute the actual received script but discard its reply and
				// close the original connection. go-redis must retry after EOF.
				var reply bytes.Buffer
				sink := server.NewPeer(bufio.NewWriter(&reply))
				mr.Server().Dispatch(sink, append([]string{cmd}, args...))
				sink.Flush()
				committed <- reply.String()
				peer.Close()
				return true
			})
			release, ok, err := acquire(ctx, "id", 1, "lost-reply")
			if release != nil {
				defer release()
			}
			if err != nil || !ok || release == nil {
				t.Fatalf("go-redis commit retry: ok=%v release=%v err=%v", ok, release != nil, err)
			}
			select {
			case reply := <-committed:
				if reply != ":1\r\n" {
					t.Fatalf("first command did not commit: %q", reply)
				}
			default:
				t.Fatal("lost-response fault not injected")
			}
			if count := c.ZCard(ctx, key).Val(); count != 1 {
				t.Fatalf("retry occupancy=%d", count)
			}
			release()
			if mr.Exists(key) {
				t.Fatal("release retained committed retry")
			}
		})
	}
}
