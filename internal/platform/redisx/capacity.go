package redisx

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const occupancyTTL = 20 * time.Millisecond
const occupancyBatch = 512
const occupancyCacheLimit = 8192

type occupancyEntry struct {
	n          int
	at         time.Time
	generation uint64
}
type occupancyFlight struct {
	done       chan struct{}
	err        error
	values     map[string]int
	generation uint64
}
type occupancyCache struct {
	mu         sync.Mutex
	entries    map[string]occupancyEntry
	flight     *occupancyFlight
	generation uint64
}

// Occupancy returns advisory live-lease counts, never admission grants. Reads
// are pipelined in bounded batches, cached briefly and coalesced across callers.
// fresh bypasses pre-existing cache entries but joins an in-progress refresh.
// A canceled caller does not cancel another caller's refresh; detached I/O has
// its own one-second bound. No Lua write, expired-member cleanup or key creation
// is necessary: ZCOUNT excludes expired lease scores without touching owners.
func (l *Limiter) Occupancy(ctx context.Context, accountIDs []string, fresh bool) (map[string]int, error) {
	out := make(map[string]int, len(accountIDs))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l == nil || l.c == nil {
		return out, nil
	}
	s := &l.capacity
	s.mu.Lock()
	generation := s.generation
	if s.flight != nil && fresh && generation > 0 {
		generation--
	}
	s.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s.mu.Lock()
		now := time.Now()
		missing := make([]string, 0, len(accountIDs))
		seen := make(map[string]bool, len(accountIDs))
		for _, uid := range accountIDs {
			if _, collected := out[uid]; collected {
				continue
			}
			if seen[uid] {
				continue
			}
			seen[uid] = true
			e, ok := s.entries[uid]
			if ok && now.Sub(e.at) < occupancyTTL && (!fresh || e.generation > generation) {
				out[uid] = e.n
			} else {
				missing = append(missing, uid)
			}
		}
		if len(missing) == 0 {
			s.mu.Unlock()
			return out, nil
		}
		f := s.flight
		if f == nil {
			s.generation++
			gen := s.generation
			f = &occupancyFlight{done: make(chan struct{}), generation: gen}
			s.flight = f
			go func() {
				opCtx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				values, err := l.readOccupancy(opCtx, missing)
				s.mu.Lock()
				if err == nil {
					if s.entries == nil || len(s.entries)+len(values) > occupancyCacheLimit {
						s.entries = make(map[string]occupancyEntry, min(len(values), occupancyCacheLimit))
					}
					at := time.Now()
					for uid, n := range values {
						if len(s.entries) >= occupancyCacheLimit {
							break
						}
						s.entries[uid] = occupancyEntry{n, at, gen}
					}
				}
				f.err, f.values = err, values
				s.flight = nil
				close(f.done)
				s.mu.Unlock()
			}()
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-f.done:
			if f.err != nil {
				return nil, f.err
			}
			// Copy completed results directly: a later unrelated flight may
			// evict cache entries before this waiter is scheduled again.
			if !fresh || f.generation > generation {
				for _, uid := range accountIDs {
					if n, ok := f.values[uid]; ok {
						out[uid] = n
					}
				}
			}
		}
	}
}

func (l *Limiter) readOccupancy(ctx context.Context, ids []string) (map[string]int, error) {
	out := make(map[string]int, len(ids))
	// Admission/renewal scores use the Redis clock. A client-clock cutoff can
	// incorrectly hide live owners on a fast replica (or count expired owners
	// on a slow one). TIME is read-only; occupancy never mutates lease state.
	serverNow, err := l.c.Time(ctx).Result()
	if err != nil {
		return nil, err
	}
	cutoff := "(" + strconv.FormatInt(serverNow.UnixMilli()-l.ttl.Milliseconds(), 10)
	for start := 0; start < len(ids); start += occupancyBatch {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(start+occupancyBatch, len(ids))
		pipe := l.c.Pipeline()
		cmds := make([]*redis.IntCmd, end-start)
		for i, uid := range ids[start:end] {
			cmds[i] = pipe.ZCount(ctx, l.prefix+uid, cutoff, "+inf")
		}
		_, err := pipe.Exec(ctx)
		if err != nil {
			return nil, err
		}
		for i, cmd := range cmds {
			out[ids[start+i]] = int(cmd.Val())
		}
	}
	return out, nil
}

// AcquireWithin bounds a single atomic admission probe without turning it into
// a queue or shortening a successful lease's request lifetime.
func (l *Limiter) AcquireWithin(ctx context.Context, accountID string, max int, member string, budget time.Duration) (func(), bool, error) {
	opCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	return l.acquireLease(opCtx, ctx, l.prefix+accountID, max, member)
}
