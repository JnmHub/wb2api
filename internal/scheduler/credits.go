package scheduler

import (
	"context"
	"math"
	"time"

	"wb2api/internal/pool"
)

const (
	creditsTick        = 30 * time.Minute
	creditsFailedParks = 6 * time.Hour
)

// stalest returns the enabled, credentialed account whose balance is oldest and
// past the TTL (a never-fetched balance counts as infinitely old, so a fresh
// install fills in its accounts before any timed-out one is revisited).
func (s *Scheduler) stalest(ttl time.Duration, now time.Time) *pool.Account {
	var best *pool.Account
	bestAge := -1.0
	for _, a := range s.Pool.All() {
		rec := a.Record()
		if !a.Enabled() || rec.AccessToken == "" {
			continue
		}
		s.mu.Lock()
		parked := s.parked[a.UID].After(now)
		s.mu.Unlock()
		if parked {
			continue
		}
		age := math.Inf(1)
		if t, ok := rec.Credits["updated_at"]; ok {
			if f, ok := num(t); ok {
				age = float64(now.UnixNano())/1e9 - f
			}
		}
		if age < ttl.Seconds() {
			continue
		}
		if best == nil || age > bestAge {
			best, bestAge = a, age
		}
	}
	return best
}

func num(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case interface{ Float64() (float64, error) }:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}

// RefreshCreditsOnce refreshes the single stalest balance past the TTL and
// returns its uid ("" when none was due). One account per tick bounds the
// billing calls at a few dozen a day however large the pool is; a failing
// account is parked for six hours so a broken credential is not retried every
// tick.
func (s *Scheduler) RefreshCreditsOnce(ctx context.Context) string {
	ttl := time.Duration(s.Settings.CreditsRefreshHours() * float64(time.Hour))
	if ttl <= 0 {
		return ""
	}
	now := s.now()
	acc := s.stalest(ttl, now)
	if acc == nil {
		return ""
	}
	res := s.Ops.FetchCredits(ctx, acc)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.parked == nil {
		s.parked = map[string]time.Time{}
	}
	if !res.OK {
		s.parked[acc.UID] = now.Add(creditsFailedParks)
		s.credits.LastErr = res.Error
		s.logsAppendLocked("刷新 " + short(acc.UID) + " 积分失败，暂缓六小时: " + res.Error)
		return ""
	}
	s.credits.LastRun, s.credits.LastUID, s.credits.LastErr = now.Format("2006-01-02 15:04:05"), acc.UID, ""
	return acc.UID
}

func (s *Scheduler) logsAppendLocked(msg string) {
	s.logs = append(s.logs, s.now().Format("2006-01-02 15:04:05")+" "+msg)
	if len(s.logs) > 60 {
		s.logs = s.logs[len(s.logs)-60:]
	}
}

// RunCredits is the background balance refresher loop.
func (s *Scheduler) RunCredits(ctx context.Context) {
	t := time.NewTicker(creditsTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.isLeader() {
				s.RefreshCreditsOnce(ctx)
			}
		}
	}
}
