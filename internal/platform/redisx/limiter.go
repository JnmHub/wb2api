package redisx

import (
	"context"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Limiter is a Redis-backed per-account in-flight gate. Entries carry a short
// TTL so a crashed process cannot leave an account permanently full.
type Limiter struct {
	c        *Client
	prefix   string
	ttl      time.Duration
	capacity occupancyCache
}

// NewLimiter returns a limiter using c. A nil c is allowed; Acquire is then a
// no-op so callers can keep Redis optional.
func NewLimiter(c *Client) *Limiter {
	return &Limiter{c: c, prefix: "wb:inflight:", ttl: 10 * time.Minute}
}

var acquireScript = redis.NewScript(`
local key = KEYS[1]
-- ARGV[1] is retained for wire compatibility, never a trusted client clock.
local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
local ttl = tonumber(ARGV[2])
redis.call('ZREMRANGEBYSCORE', key, '-inf', now - ttl)
-- A committed command may be retried after its reply is lost. An existing
-- live member already owns capacity, even if the limit has since decreased.
-- Expired members were removed above and must compete for capacity again.
if not redis.call('ZSCORE', key, ARGV[4]) and redis.call('ZCARD', key) >= tonumber(ARGV[3]) then
	return 0
end
redis.call('ZADD', key, now, ARGV[4])
redis.call('PEXPIRE', key, ttl)
return 1
`)

// Renew only an existing lease: a delayed heartbeat must never resurrect a
// released or expired slot. Account and API-key slots use the same protocol.
var renewScript = redis.NewScript(`
local key = KEYS[1]
local score = tonumber(redis.call('ZSCORE', key, ARGV[1]))
-- Lease scores and expiry share the Redis clock across all replicas.
local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
local ttl = tonumber(ARGV[3])
if not score or score <= now - ttl then
	return 0
end
redis.call('ZADD', key, 'XX', now, ARGV[1])
redis.call('PEXPIRE', key, ARGV[3])
return 1
`)

// AcquireWait retries Acquire until a slot frees, maxWait expires, or ctx is
// canceled. It is a short client-side queue: the request keeps its sticky
// account and waits for a slot instead of immediately spilling to a cold
// account. A zero maxWait is exactly Acquire.
func (l *Limiter) AcquireWait(ctx context.Context, accountID string, max int, member string, maxWait time.Duration) (func(), bool, error) {
	return l.acquireWait(ctx, l.prefix+accountID, max, member, maxWait)
}

// Acquire reserves one account slot. max <= 0 is unlimited. member identifies
// one logical acquisition and must not be shared by distinct concurrent holders.
// A retry with the same live member confirms and refreshes that lease. The
// returned release function is safe to call more than once.
func (l *Limiter) Acquire(ctx context.Context, accountID string, max int, member string) (func(), bool, error) {
	return l.acquire(ctx, l.prefix+accountID, max, member)
}

// AcquireKey reserves one API-key slot in the same Redis-sorted-set form. It
// keeps per-key concurrency shared across replicas instead of being a local
// process counter. member follows the same logical-acquisition contract as Acquire.
func (l *Limiter) AcquireKey(ctx context.Context, keyID string, max int, member string) (func(), bool, error) {
	return l.acquire(ctx, "wb:keyinflight:"+keyID, max, member)
}

// AcquireKeyWait is the bounded queue for a busy key.
func (l *Limiter) AcquireKeyWait(ctx context.Context, keyID string, max int, member string, maxWait time.Duration) (func(), bool, error) {
	return l.acquireWait(ctx, "wb:keyinflight:"+keyID, max, member, maxWait)
}

func (l *Limiter) acquireWait(ctx context.Context, key string, limit int, member string, maxWait time.Duration) (func(), bool, error) {
	if maxWait <= 0 {
		return l.acquire(ctx, key, limit, member)
	}
	// The queue budget bounds Redis I/O as well as sleeps. Keep the lease's
	// lifetime separate: canceling this helper must not stop a successful
	// acquisition's heartbeat when the helper returns.
	waitCtx, cancel := context.WithTimeout(ctx, maxWait)
	defer cancel()
	deadline, _ := waitCtx.Deadline()
	delay := 40 * time.Millisecond
	for {
		rel, ok, err := l.acquireLease(waitCtx, ctx, key, limit, member)
		if ok {
			return rel, true, nil
		}
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		if waitCtx.Err() != nil || !time.Now().Before(deadline) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		timer := time.NewTimer(delay)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return nil, false, ctx.Err()
			}
			return nil, false, nil
		case <-timer.C:
		}
		delay = min(delay*3/2, 500*time.Millisecond)
	}
}

func (l *Limiter) acquire(ctx context.Context, key string, limit int, member string) (func(), bool, error) {
	return l.acquireLease(ctx, ctx, key, limit, member)
}

func (l *Limiter) acquireLease(opCtx, leaseCtx context.Context, key string, limit int, member string) (func(), bool, error) {
	if l == nil || l.c == nil || limit <= 0 {
		return func() {}, true, nil
	}
	acquiredAt := time.Now()
	now := acquiredAt.UnixMilli()
	n, err := acquireScript.Run(opCtx, l.c.Client,
		[]string{key},
		now, l.ttl.Milliseconds(), limit, member,
	).Int()
	if err != nil {
		// The server may have committed even though no reply arrived. A
		// same-member retry can recover that lease; otherwise its TTL bounds
		// retention. Do not blindly ZREM here: a delayed command on another
		// connection could commit after cleanup, or cleanup could remove a
		// later successful recovery. Recovery I/O must not extend a wait budget.
		return nil, false, err
	}
	if n != 1 {
		return nil, false, nil
	}
	// The reply may arrive after the conservative local ownership window. Do
	// not expose that reservation even briefly; its Redis TTL bounds retention.
	// Avoid blind cleanup: a same-member recovery on another connection may
	// already own it, and cleanup I/O must not extend the caller's budget.
	if time.Since(acquiredAt) >= l.ttl-min(l.ttl/10, time.Second) {
		loseLease(leaseCtx)
		return nil, false, ErrLeaseLost
	}
	// Renew independently of request I/O while this attempt holds its slot.
	// Explicit release cancels in-flight renewal and joins the goroutine before
	// ZREM, so no heartbeat can race a release and restore the member.
	renewCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	interval := l.ttl / 3
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	watch := newLeaseWatch(leaseCtx, acquiredAt, l.ttl)
	go func() {
		defer close(done)
		defer watch.stop()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
				callCtx, cancel := context.WithTimeout(renewCtx, min(interval, 2*time.Second))
				renewedAt := time.Now()
				n, err := renewScript.Run(callCtx, l.c.Client, []string{key}, member,
					renewedAt.UnixMilli(), l.ttl.Milliseconds()).Int()
				cancel()
				if err == nil && n == 0 {
					watch.lose()
					return // ownership was lost; never recreate it
				}
				if err == nil && n == 1 {
					watch.confirm(renewedAt, l.ttl)
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			stop()
			<-done
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = l.c.ZRem(cleanupCtx, key, member).Err()
		})
	}, true, nil
}
