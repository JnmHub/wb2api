// Package sched holds the small account-health feedback loop used by sticky
// session routing. Samples are collected locally on every gateway process and
// can optionally be mirrored through Redis, so a slow account observed by one
// replica also steers traffic away on the others.
package sched

import (
	"context"
	"strings"
	"sync"
	"time"
)

const (
	alpha            = 0.2
	defaultMaxAge    = 5 * time.Minute
	defaultTTFTLimit = 15 * time.Second
	defaultErrLimit  = 0.5
	defaultSyncEvery = 2 * time.Second
)

type sample struct {
	ttft    float64 // milliseconds; <=0 means no TTFT sample yet
	err     float64 // EWMA in [0,1]
	seen    time.Time
	hasTTFT bool
	samples int
}

// SharedSample is the wire form of one account's local EWMA. Seen is the
// observation time so a reader can discard samples from a replica that stopped
// reporting.
type SharedSample struct {
	TTFT    float64   `json:"ttft"`
	Err     float64   `json:"err"`
	Seen    time.Time `json:"seen"`
	HasTTFT bool      `json:"has_ttft"`
	Samples int       `json:"samples"`
}

// SharedStore mirrors per-replica samples. Publish replaces the caller's
// snapshot; Load returns every other replica's samples keyed by account uid.
type SharedStore interface {
	Publish(ctx context.Context, replica string, samples map[string]SharedSample) error
	Load(ctx context.Context, excludeReplica string) (map[string]SharedSample, error)
}

// Stats tracks per-account latency/error feedback.
type Stats struct {
	mu       sync.Mutex
	accounts map[string]*sample
	remote   map[string]sample
	shared   SharedStore
	replica  string

	ttftLimit time.Duration
	errLimit  float64
	maxAge    time.Duration
}

// NewStats returns a tracker with the Sub2API-style defaults.
func NewStats(ttftLimit time.Duration, errLimit float64) *Stats {
	if ttftLimit <= 0 {
		ttftLimit = defaultTTFTLimit
	}
	if errLimit <= 0 || errLimit > 1 {
		errLimit = defaultErrLimit
	}
	return &Stats{
		accounts:  map[string]*sample{},
		remote:    map[string]sample{},
		ttftLimit: ttftLimit,
		errLimit:  errLimit,
		maxAge:    defaultMaxAge,
	}
}

// SetShared enables cross-replica feedback. replica must be stable for the
// lifetime of the process; it identifies this process's Redis snapshot.
func (s *Stats) SetShared(store SharedStore, replica string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.shared = store
	s.replica = replica
	s.mu.Unlock()
}

// RunShared publishes the local snapshot and refreshes peer samples until ctx
// is cancelled. It is a background loop: request handling never waits on Redis.
func (s *Stats) RunShared(ctx context.Context, every time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	store := s.shared
	s.mu.Unlock()
	if store == nil {
		return
	}
	if every <= 0 {
		every = defaultSyncEvery
	}
	s.SyncShared(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.SyncShared(ctx)
		}
	}
}

// SyncShared performs one publish/load round. It is exported for tests and for
// callers that want to control the cadence.
func (s *Stats) SyncShared(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	store := s.shared
	replica := s.replica
	local := make(map[string]SharedSample, len(s.accounts))
	for uid, st := range s.accounts {
		if st == nil || time.Since(st.seen) > s.maxAge {
			continue
		}
		local[uid] = toShared(st)
	}
	s.mu.Unlock()
	if store == nil {
		return
	}
	// Publish first: a replica's own fresh sample must be visible to peers even
	// when the peer read below fails.
	_ = store.Publish(ctx, replica, local)
	remote, err := store.Load(ctx, replica)
	if err != nil {
		return
	}
	converted := make(map[string]sample, len(remote))
	for uid, st := range remote {
		if uid == "" || st.Seen.IsZero() || time.Since(st.Seen) > s.maxAge {
			continue
		}
		converted[uid] = sample{
			ttft: st.TTFT, err: st.Err, seen: st.Seen,
			hasTTFT: st.HasTTFT, samples: st.Samples,
		}
	}
	s.mu.Lock()
	s.remote = converted
	s.mu.Unlock()
}

// Observe records one completed request. failed must be false for client
// cancellations (they say nothing about account health).
func (s *Stats) Observe(uid string, ttft time.Duration, failed bool) {
	if s == nil || uid == "" {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.accounts[uid]
	if st == nil {
		st = &sample{seen: now}
		s.accounts[uid] = st
	}
	failure := 0.0
	if failed {
		failure = 1
	}
	if st.samples == 0 {
		st.err = failure
	} else {
		st.err = alpha*failure + (1-alpha)*st.err
	}
	if ttft > 0 && !failed {
		ms := float64(ttft.Milliseconds())
		if !st.hasTTFT {
			st.ttft = ms
			st.hasTTFT = true
		} else {
			st.ttft = alpha*ms + (1-alpha)*st.ttft
		}
	}
	st.samples++
	st.seen = now
}

// Score returns the current TTFT (ms) and error EWMA for an account. ok is
// false when there is no fresh sample. The worst fresh value from any replica
// wins, so one replica cannot hide an account that is failing elsewhere.
func (s *Stats) Score(uid string) (ttft, errRate float64, ok bool) {
	if s == nil || uid == "" {
		return 0, 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.effectiveLocked(uid)
	if !ok {
		return 0, 0, false
	}
	return st.ttft, st.err, st.hasTTFT || st.samples > 0
}

// ShouldEscape reports whether the warm account should be skipped for this
// request. Stale samples do not pin an account forever.
func (s *Stats) ShouldEscape(uid string) bool {
	if s == nil || uid == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.effectiveLocked(uid)
	if !ok {
		return false
	}
	if st.hasTTFT && st.ttft > float64(s.ttftLimit.Milliseconds()) {
		return true
	}
	return st.err > s.errLimit
}

// effectiveLocked combines local and peer samples using the worst fresh value.
// Local samples remain authoritative for this process's EWMA input; peers are a
// read-only overlay, so a remote value never contaminates future observations.
func (s *Stats) effectiveLocked(uid string) (sample, bool) {
	var out sample
	found := false
	merge := func(in *sample) {
		if in == nil || time.Since(in.seen) > s.maxAge {
			return
		}
		if !found {
			out = *in
			found = true
			return
		}
		if in.hasTTFT && (!out.hasTTFT || in.ttft > out.ttft) {
			out.ttft = in.ttft
			out.hasTTFT = true
		}
		if in.err > out.err {
			out.err = in.err
		}
		if in.seen.After(out.seen) {
			out.seen = in.seen
		}
		if in.samples > out.samples {
			out.samples = in.samples
		}
	}
	merge(s.accounts[uid])
	if remote, ok := s.remote[uid]; ok {
		merge(&remote)
	}
	if !found || time.Since(out.seen) > s.maxAge {
		return sample{}, false
	}
	return out, true
}

func toShared(st *sample) SharedSample {
	if st == nil {
		return SharedSample{}
	}
	return SharedSample{
		TTFT: st.ttft, Err: st.err, Seen: st.seen,
		HasTTFT: st.hasTTFT, Samples: st.samples,
	}
}

// Failure should be computed from the gateway outcome. client_aborted is a
// client decision, not an account failure.
func Failure(outcome string) bool {
	switch strings.ToLower(strings.TrimSpace(outcome)) {
	case "client_aborted":
		return false
	default:
		return outcome != "" && outcome != "completed" && outcome != "ok"
	}
}
