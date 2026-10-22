package redisx

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

// Observations cannot be used indefinitely, including time spent in a request,
// queue, partition, or Redis retry. Metadata is bounded to one idle-expiring hash
// per conversation (not one tombstone per uid). Its retention exceeds max age.
// A random epoch on recreation additionally prevents ABA after expiry/eviction;
// no wall clock or process-local counter is used as a distributed generation.
const (
	AffinityOperationMaxAge = 2 * time.Minute
	AffinityMetadataTTL     = 10 * time.Minute
)

func NewAffinityStore(rdb *redis.Client) *AffinityStore {
	return &AffinityStore{rdb: rdb, prefix: affinityPrefix}
}

func affinityKey(prefix, key string) string {
	sum := sha256.Sum256([]byte(key))
	// Both script keys share a Redis Cluster hash tag. The client is currently
	// standalone, but the script itself never requires cross-slot operations.
	return prefix + "{" + hex.EncodeToString(sum[:])[:24] + "}"
}

// TIME is only an expiry/max-age source. Ordering is the atomic integer
// generation in Redis, combined with a random incarnation epoch.
const affinityScriptPrelude = `
local key = KEYS[1]
local meta = KEYS[2]
local clock = redis.call("TIME")
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
`

var affinityReadScript = redis.NewScript(affinityScriptPrelude + `
if redis.call("EXISTS", meta) == 0 then
  redis.call("HSET", meta, "epoch", ARGV[1], "generation", "0")
end
redis.call("PEXPIRE", meta, ARGV[2])
redis.call("ZREMRANGEBYSCORE", key, "-inf", now)
local token = redis.call("HGET", meta, "epoch") .. ":" .. redis.call("HGET", meta, "generation") .. ":" .. string.format("%.0f", now)
local result = {token}
local uids = redis.call("ZREVRANGE", key, 0, tonumber(ARGV[3]) - 1)
for _, uid in ipairs(uids) do table.insert(result, uid) end
return result
`)

var affinityBindScript = redis.NewScript(affinityScriptPrelude + `
local epoch, generation, issued = string.match(ARGV[1], "^([^:]+):([^:]+):(%d+)$")
if not issued then return 0 end
local age = now - tonumber(issued)
if age < 0 or age > tonumber(ARGV[2]) then return 0 end
if redis.call("HGET", meta, "epoch") ~= epoch or redis.call("HGET", meta, "generation") ~= generation then return 0 end
local ttl = tonumber(ARGV[4])
local history = tonumber(ARGV[5])
redis.call("ZREMRANGEBYSCORE", key, "-inf", now)
redis.call("ZADD", key, now + ttl, ARGV[3])
redis.call("ZREMRANGEBYRANK", key, 0, -(history + 1))
-- Never shorten the key TTL below an older warm entry's independent TTL.
local newest = redis.call("ZREVRANGE", key, 0, 0, "WITHSCORES")
redis.call("PEXPIRE", key, math.max(1, tonumber(newest[2]) - now + 60000))
redis.call("PEXPIRE", meta, ARGV[6])
return 1
`)

var affinityDemoteScript = redis.NewScript(affinityScriptPrelude + `
if redis.call("EXISTS", meta) == 0 then
  redis.call("HSET", meta, "epoch", ARGV[1], "generation", "0")
end
redis.call("HINCRBY", meta, "generation", 1)
redis.call("PEXPIRE", meta, ARGV[2])
redis.call("ZREM", key, ARGV[3])
return 1
`)

func (s *AffinityStore) keys(key string) []string {
	k := affinityKey(s.prefix, key)
	return []string{k, k + ":generation"}
}

func affinityEpoch() (string, error) {
	var epoch [16]byte
	if _, err := rand.Read(epoch[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(epoch[:]), nil
}

// Recent returns live accounts, most recent first. SessionAffinity uses the
// optional RecentVersion instead, preserving the read's exact observation.
func (s *AffinityStore) Recent(ctx context.Context, key string, limit int) ([]string, error) {
	uids, _, err := s.RecentVersion(ctx, key, limit)
	return uids, err
}

// RecentVersion atomically observes entries and their fencing generation. Even
// an absent result has an observation, so a subsequent successful new account
// can bind without fetching a newer generation at write time.
func (s *AffinityStore) RecentVersion(ctx context.Context, key string, limit int) ([]string, string, error) {
	if s == nil || s.rdb == nil {
		return nil, "", errors.New("affinity store is not configured")
	}
	if key == "" {
		return nil, "", nil
	}
	if limit <= 0 {
		limit = 3
	}
	limit = min(limit, 64)
	epoch, err := affinityEpoch()
	if err != nil {
		return nil, "", err
	}
	result, err := affinityReadScript.Run(ctx, s.rdb, s.keys(key), epoch, AffinityMetadataTTL.Milliseconds(), limit).StringSlice()
	if err != nil {
		return nil, "", err
	}
	if len(result) == 0 {
		return nil, "", errors.New("affinity read returned no generation")
	}
	return result[1:], result[0], nil
}

// BindVersion only commits the generation observed before successful usage.
// Rejection is a normal stale/expired-operation result, not a backend error.
// In particular, it MUST NOT fetch a fresh token and retry after rejection.
func (s *AffinityStore) BindVersion(ctx context.Context, key, uid, version string, ttl time.Duration, history int) (bool, error) {
	if s == nil || s.rdb == nil {
		return false, errors.New("affinity store is not configured")
	}
	if key == "" || uid == "" || version == "" {
		return false, nil
	}
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	if history <= 0 {
		history = 3
	}
	history = min(history, 64)
	n, err := affinityBindScript.Run(ctx, s.rdb, s.keys(key), version, AffinityOperationMaxAge.Milliseconds(), uid, max(1, ttl.Milliseconds()), history, AffinityMetadataTTL.Milliseconds()).Int()
	return n == 1, err
}

// Bind retains the old store API for direct callers. It is a new immediate
// operation, not suitable for delayed request completions: those must retain an
// observation and call BindVersion. SessionAffinity never uses this unfenced
// compatibility method when the optional versioned interface is available.
func (s *AffinityStore) Bind(ctx context.Context, key, uid string, ttl time.Duration, history int) error {
	_, version, err := s.RecentVersion(ctx, key, 1)
	if err != nil {
		return err
	}
	_, err = s.BindVersion(ctx, key, uid, version, ttl, history)
	return err
}

// Demote atomically advances the conversation generation and removes uid. This
// fences all earlier observations, conservatively including other warm accounts.
func (s *AffinityStore) Demote(ctx context.Context, key, uid string) error {
	if s == nil || s.rdb == nil {
		return errors.New("affinity store is not configured")
	}
	if key == "" || uid == "" {
		return nil
	}
	epoch, err := affinityEpoch()
	if err != nil {
		return err
	}
	return affinityDemoteScript.Run(ctx, s.rdb, s.keys(key), epoch, AffinityMetadataTTL.Milliseconds(), uid).Err()
}
