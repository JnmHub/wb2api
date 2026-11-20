package raccoonsync

import (
	"context"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/upstream/raccoon"
)

// DefaultInterval is how often the background scanner refreshes one account's
// catalogue. The raccoon catalogue changes on plan rotation, not by the minute,
// and every pass costs one authenticated call.
const DefaultInterval = 30 * time.Minute

// Scanner keeps every raccoon account's model catalogue fresh. Discovery writes
// the account record; /v1/models reads whatever is stored, so the request path
// never waits on the upstream.
type Scanner struct {
	Service *Service
	// Interval is the per-account refresh period (0 = DefaultInterval).
	Interval time.Duration
	// Tick is the scan period (0 = min(Interval/6, 5m)).
	Tick time.Duration
	Now  func() time.Time
	// Leader prevents every replica from discovering the same shared accounts.
	Leader func() bool
}

func (s *Scanner) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
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
		if ctx.Err() != nil || (s.Leader != nil && !s.Leader()) {
			break
		}
		if !s.ModelRefreshDue(acc, now) {
			continue
		}
		n++
		if res := s.Service.Discover(ctx, acc); res.Err != nil {
			s.Service.logger().Warn("raccoon model discovery failed",
				"account", acc.UID, "err", res.Err)
		}
	}
	return n
}

// ModelRefreshDue reports whether one account's catalogue is stale.
func (s *Scanner) ModelRefreshDue(acc *pool.Account, now time.Time) bool {
	if acc == nil {
		return false
	}
	rec := acc.Record()
	if rec == nil || !realm.IsRaccoon(rec.Realm) || !rec.IsEnabled() || rec.RaccoonNeedsLogin() {
		return false
	}
	cred := raccoon.CredentialOf(rec)
	if cred.Token == "" {
		return false
	}
	if !raccoon.AccessUsable(rec, now) && (cred.RefreshToken == "" || now.Before(rec.RaccoonAuthState().RetryAt)) {
		return false
	}
	at := raccoon.ModelsDiscoveredAt(rec)
	if at == 0 {
		return true
	}
	return now.Sub(time.Unix(at, 0)) >= s.interval()
}
