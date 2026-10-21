// Package sched holds the small account-health feedback loop used by sticky
// session routing. It is intentionally in-memory in this phase: one gateway
// process owns the samples, and the router can escape a warm account when it
// becomes slow or error-prone. A later phase mirrors these samples to Redis.
package sched

import (
	"strings"
	"sync"
	"time"
)

const (
	alpha            = 0.2
	defaultMaxAge    = 5 * time.Minute
	defaultTTFTLimit = 15 * time.Second
	defaultErrLimit  = 0.5
)

type sample struct {
	ttft    float64 // milliseconds; <=0 means no TTFT sample yet
	err     float64 // EWMA in [0,1]
	seen    time.Time
	hasTTFT bool
	samples int
}

// Stats tracks per-account latency/error feedback.
type Stats struct {
	mu        sync.Mutex
	accounts  map[string]*sample
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
	return &Stats{accounts: map[string]*sample{}, ttftLimit: ttftLimit, errLimit: errLimit, maxAge: defaultMaxAge}
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
// false when there is no fresh sample.
func (s *Stats) Score(uid string) (ttft, errRate float64, ok bool) {
	if s == nil || uid == "" {
		return 0, 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.accounts[uid]
	if st == nil || time.Since(st.seen) > s.maxAge {
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
	st := s.accounts[uid]
	if st == nil || time.Since(st.seen) > s.maxAge {
		return false
	}
	if st.hasTTFT && st.ttft > float64(s.ttftLimit.Milliseconds()) {
		return true
	}
	return st.err > s.errLimit
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
