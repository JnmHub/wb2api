package redisx

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"

	"wb2api/internal/sched"
)

// StatsStore mirrors scheduler EWMA snapshots through Redis. Each gateway
// process owns one hash key; the hash expires if that process stops publishing,
// so a dead replica cannot pin a slow-account score forever.
type StatsStore struct {
	rdb    *redis.Client
	prefix string
}

const (
	statsPrefix = "wb:sched:"
	statsTTL    = 30 * time.Second
)

func NewStatsStore(rdb *redis.Client) *StatsStore {
	return &StatsStore{rdb: rdb, prefix: statsPrefix}
}

func statsKey(prefix, replica string) string {
	return prefix + replica
}

// Publish replaces the replica snapshot atomically. An empty snapshot deletes
// the key instead of leaving stale fields behind.
func (s *StatsStore) Publish(ctx context.Context, replica string, samples map[string]sched.SharedSample) error {
	if s == nil || s.rdb == nil {
		return errors.New("stats store is not configured")
	}
	if replica == "" {
		return errors.New("stats store replica is empty")
	}
	key := statsKey(s.prefix, replica)
	if len(samples) == 0 {
		return s.rdb.Del(ctx, key).Err()
	}
	pipe := s.rdb.TxPipeline()
	pipe.Del(ctx, key)
	for uid, st := range samples {
		if uid == "" {
			continue
		}
		raw, err := json.Marshal(st)
		if err != nil {
			continue
		}
		pipe.HSet(ctx, key, uid, raw)
	}
	pipe.Expire(ctx, key, statsTTL)
	_, err := pipe.Exec(ctx)
	return err
}

// Load merges every other replica's snapshot by taking the worst fresh value
// per account. excludeReplica keeps a process from reading its own stale copy.
func (s *StatsStore) Load(ctx context.Context, excludeReplica string) (map[string]sched.SharedSample, error) {
	if s == nil || s.rdb == nil {
		return nil, errors.New("stats store is not configured")
	}
	exclude := ""
	if excludeReplica != "" {
		exclude = statsKey(s.prefix, excludeReplica)
	}
	out := map[string]sched.SharedSample{}
	var cursor uint64
	for {
		keys, next, err := s.rdb.Scan(ctx, cursor, s.prefix+"*", 100).Result()
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			if key == exclude {
				continue
			}
			fields, err := s.rdb.HGetAll(ctx, key).Result()
			if err != nil {
				return nil, err
			}
			for uid, raw := range fields {
				var st sched.SharedSample
				if err := json.Unmarshal([]byte(raw), &st); err != nil {
					continue
				}
				if st.Seen.IsZero() {
					continue
				}
				if prev, ok := out[uid]; ok {
					st = worseSample(prev, st)
				}
				out[uid] = st
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return out, nil
}

func worseSample(a, b sched.SharedSample) sched.SharedSample {
	out := a
	if b.HasTTFT && (!out.HasTTFT || b.TTFT > out.TTFT) {
		out.TTFT = b.TTFT
		out.HasTTFT = true
	}
	if b.Err > out.Err {
		out.Err = b.Err
	}
	if b.Seen.After(out.Seen) {
		out.Seen = b.Seen
	}
	if b.Samples > out.Samples {
		out.Samples = b.Samples
	}
	return out
}
