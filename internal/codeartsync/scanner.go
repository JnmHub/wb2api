package codeartsync

import (
	"context"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/upstream/codearts"
)

// DefaultInterval is how often the background scanner refreshes one account's
// catalogue. The upstream catalogue changes on plan rotation, not by the
// minute, and every pass costs three signed calls.
const DefaultInterval = 30 * time.Minute

// Scanner keeps every CodeArts account's model catalogue fresh. It never blocks
// the request path: discovery writes the account record, and /v1/models reads
// whatever is stored.
type Scanner struct {
	Service *Service
	// Interval is the per-account refresh period (0 = DefaultInterval).
	Interval time.Duration
	// Tick is the scan period (0 = min(Interval, 5m)).
	Tick time.Duration
	// Now is the clock (tests).
	Now func() time.Time
}

func (s *Scanner) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	// One clock for the subsystem: the service stamps models_at and the scanner
	// decides what is stale, so both must read the same time source.
	if s.Service != nil && s.Service.Now != nil {
		return s.Service.Now()
	}
	return time.Now()
}

func (s *Scanner) interval() time.Duration {
	if s.Interval > 0 {
		return s.Interval
	}
	return DefaultInterval
}

func (s *Scanner) tick() time.Duration {
	if s.Tick > 0 {
		return s.Tick
	}
	if d := s.interval() / 6; d < 5*time.Minute {
		return d
	}
	return 5 * time.Minute
}

// Run scans until ctx is cancelled.
func (s *Scanner) Run(ctx context.Context) {
	t := time.NewTicker(s.tick())
	defer t.Stop()
	s.ScanOnce(ctx) // one pass at start: a restart must not wait a full period
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.ScanOnce(ctx)
		}
	}
}

// ScanOnce refreshes every due account and returns how many were attempted.
func (s *Scanner) ScanOnce(ctx context.Context) int {
	if s.Service == nil || s.Service.Pool == nil {
		return 0
	}
	now := s.now()
	n := 0
	for _, acc := range s.Service.Pool.All() {
		if ctx.Err() != nil {
			break
		}
		if !s.due(acc, now) {
			continue
		}
		n++
		if _, err := s.Service.Discover(ctx, acc); err != nil {
			s.Service.logger().Warn("codearts model discovery failed", "account", short(acc.UID), "err", err)
		}
	}
	return n
}

// ModelRefreshDue reports whether one account's catalogue is stale.
func (s *Scanner) ModelRefreshDue(acc *pool.Account) bool { return s.due(acc, s.now()) }

func (s *Scanner) due(acc *pool.Account, now time.Time) bool {
	if acc == nil {
		return false
	}
	rec := acc.Record()
	if rec == nil || !realm.IsCodeArts(rec.Realm) || !rec.IsEnabled() {
		return false
	}
	if rec.AccessToken == "" {
		return false
	}
	m := codearts.Extra(rec)
	if m == nil {
		return true
	}
	at, _ := m["models_at"].(float64)
	if at == 0 {
		return true
	}
	return now.Sub(time.Unix(int64(at), 0)) >= s.interval()
}

func short(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}
