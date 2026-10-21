package redisx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// AffinityStore shares conversation-to-account bindings between replicas. Keys
// are hashed so a client-supplied session id never appears in Redis.
type AffinityStore struct {
	rdb    *redis.Client
	prefix string
}

const affinityPrefix = "wb:aff:"

func NewAffinityStore(rdb *redis.Client) *AffinityStore {
	return &AffinityStore{rdb: rdb, prefix: affinityPrefix}
}

func affinityKey(prefix, key string) string {
	sum := sha256.Sum256([]byte(key))
	return prefix + hex.EncodeToString(sum[:])[:24]
}

var affinityBindScript = redis.NewScript(`
local key = KEYS[1]
local now = tonumber(ARGV[1])
local expires = tonumber(ARGV[2])
local uid = ARGV[3]
local history = tonumber(ARGV[4])
local ttl = tonumber(ARGV[5])
redis.call("ZREMRANGEBYSCORE", key, "-inf", now)
redis.call("ZADD", key, expires, uid)
redis.call("ZREMRANGEBYRANK", key, 0, -(history + 1))
redis.call("PEXPIRE", key, ttl)
return 1
`)

// Recent returns the conversation's live accounts, most recent first.
func (s *AffinityStore) Recent(ctx context.Context, key string, limit int) ([]string, error) {
	if s == nil || s.rdb == nil {
		return nil, errors.New("affinity store is not configured")
	}
	if key == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 3
	}
	if limit > 64 {
		limit = 64
	}
	k := affinityKey(s.prefix, key)
	now := time.Now().UnixMilli()
	pipe := s.rdb.Pipeline()
	pipe.ZRemRangeByScore(ctx, k, "-inf", strconv.FormatInt(now, 10))
	cmd := pipe.ZRevRange(ctx, k, 0, int64(limit-1))
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	return cmd.Result()
}

// Bind makes uid the front entry and refreshes the conversation TTL.
func (s *AffinityStore) Bind(ctx context.Context, key, uid string, ttl time.Duration, history int) error {
	if s == nil || s.rdb == nil {
		return errors.New("affinity store is not configured")
	}
	if key == "" || uid == "" {
		return nil
	}
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	if history <= 0 {
		history = 3
	}
	now := time.Now()
	// Keep the key slightly longer than its newest entry so a quiet
	// conversation still expires without leaking memory.
	keyTTL := ttl + time.Minute
	_, err := affinityBindScript.Run(ctx, s.rdb,
		[]string{affinityKey(s.prefix, key)},
		now.UnixMilli(), now.Add(ttl).UnixMilli(), uid, history, keyTTL.Milliseconds()).Result()
	return err
}

// Demote removes one account from a conversation on every replica.
func (s *AffinityStore) Demote(ctx context.Context, key, uid string) error {
	if s == nil || s.rdb == nil {
		return errors.New("affinity store is not configured")
	}
	if key == "" || uid == "" {
		return nil
	}
	return s.rdb.ZRem(ctx, affinityKey(s.prefix, key), uid).Err()
}
