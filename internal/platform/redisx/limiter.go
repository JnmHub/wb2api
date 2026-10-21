package redisx

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// Limiter is a Redis-backed per-account in-flight gate. Entries carry a short
// TTL so a crashed process cannot leave an account permanently full.
type Limiter struct {
	c      *Client
	prefix string
	ttl    time.Duration
}

// NewLimiter returns a limiter using c. A nil c is allowed; Acquire is then a
// no-op so callers can keep Redis optional.
func NewLimiter(c *Client) *Limiter {
	return &Limiter{c: c, prefix: "wb:inflight:", ttl: 10 * time.Minute}
}

var acquireScript = redis.NewScript(`
local key = KEYS[1]
local now = tonumber(ARGV[1])
local ttl = tonumber(ARGV[2])
redis.call('ZREMRANGEBYSCORE', key, '-inf', now - ttl)
if redis.call('ZCARD', key) >= tonumber(ARGV[3]) then
	return 0
end
redis.call('ZADD', key, now, ARGV[4])
redis.call('PEXPIRE', key, ttl)
return 1
`)

// AcquireWait retries Acquire until a slot frees, maxWait expires, or ctx is
// canceled. It is a short client-side queue: the request keeps its sticky
// account and waits for a slot instead of immediately spilling to a cold
// account. A zero maxWait is exactly Acquire.
func (l *Limiter) AcquireWait(ctx context.Context, accountID string, max int, member string, maxWait time.Duration) (func(), bool, error) {
	if maxWait <= 0 {
		return l.Acquire(ctx, accountID, max, member)
	}
	deadline := time.Now().Add(maxWait)
	delay := 40 * time.Millisecond
	for {
		rel, ok, err := l.Acquire(ctx, accountID, max, member)
		if err != nil || ok {
			return rel, ok, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, false, nil
		}
		if delay > remaining {
			delay = remaining
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, false, ctx.Err()
		case <-timer.C:
		}
		if delay < 500*time.Millisecond {
			delay = delay * 3 / 2
		}
	}
}

// Acquire reserves one account slot. max <= 0 is unlimited. The returned
// release function is safe to call once.
func (l *Limiter) Acquire(ctx context.Context, accountID string, max int, member string) (func(), bool, error) {
	return l.acquire(ctx, l.prefix+accountID, max, member)
}

// AcquireKey reserves one API-key slot in the same Redis-sorted-set form. It
// keeps per-key concurrency shared across replicas instead of being a local
// process counter.
func (l *Limiter) AcquireKey(ctx context.Context, keyID string, max int, member string) (func(), bool, error) {
	return l.acquire(ctx, "wb:keyinflight:"+keyID, max, member)
}

// AcquireKeyWait is the bounded queue for a busy key.
func (l *Limiter) AcquireKeyWait(ctx context.Context, keyID string, max int, member string, maxWait time.Duration) (func(), bool, error) {
	if maxWait <= 0 {
		return l.AcquireKey(ctx, keyID, max, member)
	}
	deadline := time.Now().Add(maxWait)
	delay := 40 * time.Millisecond
	for {
		rel, ok, err := l.AcquireKey(ctx, keyID, max, member)
		if err != nil || ok {
			return rel, ok, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, false, nil
		}
		if delay > remaining {
			delay = remaining
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, false, ctx.Err()
		case <-timer.C:
		}
		if delay < 500*time.Millisecond {
			delay = delay * 3 / 2
		}
	}
}

func (l *Limiter) acquire(ctx context.Context, key string, max int, member string) (func(), bool, error) {
	if l == nil || l.c == nil || max <= 0 {
		return func() {}, true, nil
	}
	now := time.Now().UnixMilli()
	n, err := acquireScript.Run(ctx, l.c.Client,
		[]string{key},
		now, l.ttl.Milliseconds(), max, member,
	).Int()
	if err != nil {
		return nil, false, err
	}
	if n != 1 {
		return nil, false, nil
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = l.c.ZRem(ctx, key, member).Err()
	}, true, nil
}
