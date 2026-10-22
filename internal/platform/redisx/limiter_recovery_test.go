package redisx

import (
	"bufio"
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
)

func TestLimiterUnconfirmedCommitRecovery(t *testing.T) {
	for _, namespace := range []string{"account", "key"} {
		for _, waiting := range []bool{false, true} {
			name := namespace + "/request-deadline"
			if waiting {
				name = namespace + "/queue-deadline"
			}
			t.Run(name, func(t *testing.T) {
				mr := miniredis.RunT(t)
				ctx := context.Background()
				c, err := Open(ctx, mr.Addr(), "", 0)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				l := NewLimiter(c)
				acquire, wait, key := l.Acquire, l.AcquireWait, l.prefix+"id"
				if namespace == "key" {
					acquire, wait, key = l.AcquireKey, l.AcquireKeyWait, "wb:keyinflight:id"
				}
				if err := acquireScript.Load(ctx, c.Client).Err(); err != nil {
					t.Fatal(err)
				}
				var injected atomic.Bool
				committed := make(chan string, 1)
				mr.Server().SetPreHook(func(_ *server.Peer, cmd string, args ...string) bool {
					if cmd != "EVALSHA" || !injected.CompareAndSwap(false, true) {
						return false
					}
					var reply bytes.Buffer
					sink := server.NewPeer(bufio.NewWriter(&reply))
					mr.Server().Dispatch(sink, append([]string{cmd}, args...))
					sink.Flush()
					committed <- reply.String()
					return true // commit, but withhold response until the I/O deadline
				})
				start := time.Now()
				var release func()
				var ok bool
				if waiting {
					release, ok, err = wait(ctx, "id", 1, "unconfirmed", 100*time.Millisecond)
				} else {
					opCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
					defer cancel()
					release, ok, err = acquire(opCtx, "id", 1, "unconfirmed")
				}
				if release != nil {
					defer release()
				}
				if ok || release != nil || (waiting && err != nil) || (!waiting && err == nil) {
					t.Fatalf("unconfirmed result: ok=%v release=%v err=%v", ok, release != nil, err)
				}
				if elapsed := time.Since(start); elapsed > time.Second {
					t.Fatalf("unconfirmed acquisition exceeded deadline: %v", elapsed)
				}
				select {
				case reply := <-committed:
					if reply != ":1\r\n" {
						t.Fatalf("first command did not commit: %q", reply)
					}
				default:
					t.Fatal("fault not injected")
				}
				// No success can be reported without confirmation. Retain the
				// bounded lease rather than admitting a distinct holder or issuing
				// an unsafe cross-connection cleanup of an uncertain command.
				if count := c.ZCard(ctx, key).Val(); count != 1 || mr.TTL(key) != l.ttl {
					t.Fatalf("unconfirmed lease must remain TTL-bounded: count=%d ttl=%v", count, mr.TTL(key))
				}
				other, ok, err := acquire(ctx, "id", 1, "other")
				if other != nil {
					other()
				}
				if err != nil || ok {
					t.Fatalf("distinct holder admitted over unconfirmed lease: ok=%v err=%v", ok, err)
				}
				release, ok, err = acquire(ctx, "id", 1, "unconfirmed")
				if release != nil {
					defer release()
				}
				if err != nil || !ok || release == nil {
					t.Fatalf("later recovery: ok=%v err=%v", ok, err)
				}
				release()
				if mr.Exists(key) {
					t.Fatal("recovery release retained lease")
				}
			})
		}
	}
}

func TestLimiterRenewDoesNotResurrectReleasedOrExpired(t *testing.T) {
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
			acquire := func(member string) {
				t.Helper()
				mr.SetTime(time.UnixMilli(now))
				if n, err := acquireScript.Run(ctx, c.Client, []string{key}, now, ttl, 1, member).Int(); err != nil || n != 1 {
					t.Fatalf("acquire: n=%d err=%v", n, err)
				}
			}
			renew := func(member string, at int64, want int) {
				t.Helper()
				mr.SetTime(time.UnixMilli(at))
				if n, err := renewScript.Run(ctx, c.Client, []string{key}, member, at, ttl).Int(); err != nil || n != want {
					t.Fatalf("renew(%q, %d): n=%d want=%d err=%v", member, at, n, want, err)
				}
			}
			acquire("held")
			mr.SetTTL(key, time.Second)
			renew("held", now+1, 1)
			if score := c.ZScore(ctx, key, "held").Val(); score != float64(now+1) {
				t.Fatalf("renew score=%v", score)
			}
			if got := mr.TTL(key); got != time.Duration(ttl)*time.Millisecond {
				t.Fatalf("renew TTL=%v", got)
			}
			// A release before a delayed renewal cannot recreate the member.
			if err := c.ZRem(ctx, key, "held").Err(); err != nil {
				t.Fatal(err)
			}
			renew("held", now+2, 0)
			if mr.Exists(key) {
				t.Fatal("renew resurrected released key")
			}
			acquire("held")
			mr.SetTTL(key, time.Second)
			renew("held", now+ttl, 0) // inclusive expiry matches acquire cleanup
			if score := c.ZScore(ctx, key, "held").Val(); score != float64(now) || mr.TTL(key) != time.Second {
				t.Fatalf("expired renewal refreshed lease: score=%v ttl=%v", score, mr.TTL(key))
			}
			mr.FastForward(time.Second)
			renew("held", now+ttl+1, 0)
			if mr.Exists(key) {
				t.Fatal("renew resurrected expired key")
			}
		})
	}
}
