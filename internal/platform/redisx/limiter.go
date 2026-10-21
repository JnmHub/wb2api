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

// Acquire reserves one slot. max <= 0 is unlimited. The returned release
// function is safe to call once.
func (l *Limiter) Acquire(ctx context.Context, accountID string, max int, member string) (func(), bool, error) {
	if l == nil || l.c == nil || max <= 0 {
		return func() {}, true, nil
	}
	now := time.Now().UnixMilli()
	n, err := acquireScript.Run(ctx, l.c.Client,
		[]string{l.prefix + accountID},
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
		_ = l.c.ZRem(ctx, l.prefix+accountID, member).Err()
	}, true, nil
}
