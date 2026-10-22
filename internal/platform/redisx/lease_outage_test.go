package redisx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// These tests are opt-in and never flush a database or restart Redis. Every
// acquired key has a random test namespace and is deleted individually.
func leaseOutageRedis(t *testing.T) (string, *Client, string) {
	t.Helper()
	addr := os.Getenv("WB_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set WB_TEST_REDIS_ADDR to an isolated test Redis for real-network lease tests")
	}
	client := &Client{Client: redis.NewClient(&redis.Options{
		Addr: addr, DialTimeout: time.Second, ReadTimeout: time.Second,
		WriteTimeout: time.Second, ContextTimeoutEnabled: true, MaxRetries: -1,
	})}
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("WB_TEST_REDIS_ADDR unavailable: %v", err)
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	return addr, client, "wb:test:lease-outage:" + hex.EncodeToString(id[:]) + ":"
}

// replyFaultProxy forwards requests even during a reply blackout. Thus Redis
// may renew ownership successfully while the client has no confirmation. The
// local safety watch must still fence the owner; server success alone is not an
// acknowledgment. It also exercises socket I/O rather than mocking a Lua result.
type replyFaultProxy struct {
	listener net.Listener
	upstream string
	mu       sync.Mutex
	blocked  bool
	closed   bool
	lastAck  time.Time
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
}

func newReplyFaultProxy(t *testing.T, upstream string) *replyFaultProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &replyFaultProxy{listener: listener, upstream: upstream, conns: make(map[net.Conn]struct{})}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				_ = client.Close()
				return
			}
			p.conns[client] = struct{}{}
			p.wg.Add(1)
			p.mu.Unlock()
			go p.serve(client)
		}
	}()
	t.Cleanup(func() {
		p.mu.Lock()
		p.closed = true
		_ = p.listener.Close()
		for conn := range p.conns {
			_ = conn.Close()
		}
		p.mu.Unlock()
		p.wg.Wait()
	})
	return p
}

func (p *replyFaultProxy) serve(client net.Conn) {
	defer p.wg.Done()
	defer func() {
		_ = client.Close()
		p.mu.Lock()
		delete(p.conns, client)
		p.mu.Unlock()
	}()
	server, err := net.DialTimeout("tcp", p.upstream, time.Second)
	if err != nil {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = server.Close()
		return
	}
	p.conns[server] = struct{}{}
	p.mu.Unlock()
	defer func() {
		_ = server.Close()
		p.mu.Lock()
		delete(p.conns, server)
		p.mu.Unlock()
	}()
	copied := make(chan struct{})
	go func() {
		_, _ = io.Copy(server, client)
		_ = server.Close()
		close(copied)
	}()
	buf := make([]byte, 32768)
	for {
		n, err := server.Read(buf)
		if n > 0 {
			p.mu.Lock()
			blocked := p.blocked
			if !blocked {
				p.lastAck = time.Now()
			}
			p.mu.Unlock()
			if !blocked {
				if _, err := client.Write(buf[:n]); err != nil {
					break
				}
			}
		}
		if err != nil {
			break
		}
	}
	_ = client.Close()
	<-copied
}

func (p *replyFaultProxy) blackOutReplies() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.blocked = true
	return p.lastAck
}

func TestLeaseOutageRealRedisRenewalZeroCancelsOwner(t *testing.T) {
	for _, apiKey := range []bool{false, true} {
		t.Run(map[bool]string{false: "account", true: "api-key"}[apiKey], func(t *testing.T) {
			_, client, prefix := leaseOutageRedis(t)
			limiter := NewLimiter(client)
			limiter.ttl = 600 * time.Millisecond
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			ctx = limiter.WithLeaseLoss(ctx, cancel)
			key := prefix + "slot"
			// acquireLease takes the exact namespaced Redis key. Acquire and
			// AcquireKey both delegate here, sharing the ownership protocol.
			if apiKey {
				key = prefix + "key-slot"
			}
			t.Cleanup(func() { _ = client.Del(context.Background(), key).Err() })
			release, ok, err := limiter.acquireLease(ctx, ctx, key, 1, "owner")
			if err != nil || !ok {
				t.Fatalf("acquire=%v err=%v", ok, err)
			}
			defer release()
			if err := client.ZRem(context.Background(), key, "owner").Err(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(limiter.ttl):
				t.Fatal("renewal zero did not cancel owner before lease TTL")
			}
			if !errors.Is(context.Cause(ctx), ErrLeaseLost) {
				t.Fatalf("cause=%v", context.Cause(ctx))
			}
			if n, err := client.ZCard(context.Background(), key).Result(); err != nil || n != 0 {
				t.Fatalf("heartbeat resurrected removed owner: count=%d err=%v", n, err)
			}
		})
	}
}

func TestLeaseOutageRealRedisUnconfirmedRepliesFenceBeforeTTL(t *testing.T) {
	addr, control, prefix := leaseOutageRedis(t)
	proxy := newReplyFaultProxy(t, addr)
	client := &Client{Client: redis.NewClient(&redis.Options{
		Addr: proxy.listener.Addr().String(), DialTimeout: time.Second,
		ReadTimeout: 2 * time.Second, WriteTimeout: time.Second,
		ContextTimeoutEnabled: true, MaxRetries: -1,
	})}
	t.Cleanup(func() { _ = client.Close() })
	// Load first so the first blacked-out EVALSHA actually commits a renewal,
	// rather than waiting on a dropped NOSCRIPT response before EVAL fallback.
	if err := renewScript.Load(context.Background(), control.Client).Err(); err != nil {
		t.Fatal(err)
	}
	limiter := NewLimiter(client)
	limiter.ttl = 900 * time.Millisecond
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	lost := make(chan time.Time, 1)
	ctx = limiter.WithLeaseLoss(ctx, func(err error) {
		at := time.Now()
		cancel(err)
		select {
		case lost <- at:
		default:
		}
	})
	key := prefix + "blackout"
	t.Cleanup(func() { _ = control.Del(context.Background(), key).Err() })
	release, ok, err := limiter.acquireLease(ctx, ctx, key, 1, "owner")
	if err != nil || !ok {
		t.Fatalf("acquire=%v err=%v", ok, err)
	}
	defer release()
	// Healthy acknowledgments must extend the watch for multiple original TTLs.
	healthy := time.NewTimer(2 * limiter.ttl)
	defer healthy.Stop()
	select {
	case <-ctx.Done():
		t.Fatalf("healthy acknowledged renewals lost ownership: %v", context.Cause(ctx))
	case <-healthy.C:
	}
	beforeBlackout, err := control.ZScore(context.Background(), key, "owner").Result()
	if err != nil {
		t.Fatal(err)
	}
	lastAck := proxy.blackOutReplies()
	if lastAck.IsZero() {
		t.Fatal("proxy observed no confirmed replies")
	}
	var canceledAt time.Time
	select {
	case canceledAt = <-lost:
	case <-time.After(limiter.ttl + time.Second):
		t.Fatal("ambiguous renewal did not fence owner")
	}
	if !errors.Is(context.Cause(ctx), ErrLeaseLost) {
		t.Fatalf("cause=%v", context.Cause(ctx))
	}
	// Prove this was ambiguous success, not merely a disconnected socket:
	// Redis processed at least one renewal whose reply the owner never saw.
	afterBlackout, err := control.ZScore(context.Background(), key, "owner").Result()
	if err != nil || afterBlackout <= beforeBlackout {
		t.Fatalf("fault proxy did not commit an unacknowledged renewal: before=%v after=%v err=%v", beforeBlackout, afterBlackout, err)
	}
	// Forwarded ACK time is later than call-start, so this is a conservative
	// upper bound: the safety watch must not consume even that full lease TTL.
	if elapsed := canceledAt.Sub(lastAck); elapsed >= limiter.ttl {
		t.Fatalf("canceled too late after last confirmed reply: %v >= %v", elapsed, limiter.ttl)
	}
}

func TestLeaseOutageRealRedisReleaseIdempotentStopsCallbacks(t *testing.T) {
	_, client, prefix := leaseOutageRedis(t)
	limiter := NewLimiter(client)
	limiter.ttl = 300 * time.Millisecond
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	ctx = limiter.WithLeaseLoss(ctx, cancel)
	key := prefix + "release"
	t.Cleanup(func() { _ = client.Del(context.Background(), key).Err() })
	release, ok, err := limiter.acquireLease(ctx, ctx, key, 1, "owner")
	if err != nil || !ok {
		t.Fatalf("acquire=%v err=%v", ok, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); release() }()
	}
	wg.Wait()
	timer := time.NewTimer(2 * limiter.ttl)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		t.Fatalf("released lease callback canceled live request: %v", context.Cause(ctx))
	case <-timer.C:
	}
	if n, err := client.ZCard(context.Background(), key).Result(); err != nil || n != 0 {
		t.Fatalf("released lease returned: count=%d err=%v", n, err)
	}
}
