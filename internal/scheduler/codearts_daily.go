package scheduler

import (
	"context"
	"slices"
	"sync/atomic"
	"time"

	"wb2api/internal/codeartsync"
	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/settings"
)

var beijing = time.FixedZone("Asia/Shanghai", 8*60*60)

type codeArtsRun struct {
	running atomic.Bool
	lastRun time.Time // guarded by Scheduler.mu
}

type CodeArtsDailyState struct {
	settings.CodeArtsDailyConfig
	Available    bool           `json:"available"`
	Running      bool           `json:"running"`
	LastRun      string         `json:"last_run_at,omitempty"`
	NextRun      string         `json:"next_run_at,omitempty"`
	AccountCount int            `json:"account_count"`
	States       map[string]int `json:"states"`
}

func dailyStart(now time.Time, scheduled string) time.Time {
	local := now.In(beijing)
	at, _ := time.Parse("15:04", scheduled)
	return time.Date(local.Year(), local.Month(), local.Day(), at.Hour(), at.Minute(), 0, 0, beijing)
}

func (s *Scheduler) codeArtsAccounts(cfg settings.CodeArtsDailyConfig) []*pool.Account {
	if s.Pool == nil {
		return nil
	}
	uidSet := make(map[string]bool, len(cfg.UIDs))
	for _, uid := range cfg.UIDs {
		uidSet[uid] = true
	}
	var list []*pool.Account
	for _, acc := range s.Pool.All() {
		rec := acc.Record()
		if realm.IsCodeArts(rec.Realm) && rec.IsEnabled() && rec.TasksOn() && (len(uidSet) == 0 || uidSet[acc.UID]) {
			list = append(list, acc)
		}
	}
	return list
}

func (s *Scheduler) codeArtsTaskAllowed(acc *pool.Account, now time.Time) bool {
	return !acc.RestrictedUntil().After(now) && (s.Hold == nil || s.Hold(acc.Realm) <= 0)
}

// TriggerCodeArts launches only the CodeArts account scope. A manual run may
// run before the daily start time or while automatic polling is disabled.
func (s *Scheduler) TriggerCodeArts() bool { return s.runCodeArts(context.Background(), true) }

func (s *Scheduler) runCodeArts(ctx context.Context, manual bool) bool {
	if s.Channel == nil || s.daily.running.Load() {
		return false
	}
	cfg := s.Settings.CodeArtsDaily()
	now := s.now()
	start := dailyStart(now, cfg.Time)
	if !manual && (!s.isLeader() || !s.Settings.SchedulerEnabled() || !cfg.Enabled || now.Before(start)) {
		return false
	}
	list := s.codeArtsAccounts(cfg)
	if !manual {
		due := false
		for _, acc := range list {
			if s.codeArtsTaskAllowed(acc, now) && s.Channel.CanCheckin(acc) {
				due = true
				break
			}
		}
		if !due {
			return false
		}
	}
	if !s.daily.running.CompareAndSwap(false, true) {
		return false
	}
	s.mu.Lock()
	s.daily.lastRun = now
	s.mu.Unlock()
	local := now.In(beijing)
	midnight := time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, beijing)
	trigger := "scheduler"
	if manual {
		trigger = "manual"
	} else if now.Sub(start) > 2*time.Minute {
		trigger = "makeup"
	}
	go func() {
		defer s.daily.running.Store(false)
		first := true
		for _, acc := range list {
			now := s.now()
			if ctx.Err() != nil || !now.Before(midnight) {
				// Keep a still-running sweep from picking up a new Beijing day.
				return
			}
			if !manual {
				current := s.Settings.CodeArtsDaily()
				if !s.isLeader() || !s.Settings.SchedulerEnabled() || !current.Enabled {
					return
				}
				if len(current.UIDs) > 0 && !slices.Contains(current.UIDs, acc.UID) {
					continue
				}
			}
			if !s.codeArtsTaskAllowed(acc, now) || !s.Channel.CanCheckin(acc) {
				continue
			}
			if !first {
				select {
				case <-time.After(s.stagger()):
				case <-ctx.Done():
					return
				}
			}
			first = false
			now = s.now()
			if !now.Before(midnight) {
				return
			}
			result, err := s.Channel.RunDaily(ctx, acc, trigger)
			if err != nil {
				s.logger().Warn("CodeArts 签到执行异常", "account", short(acc.UID), "state", result.State, "message", result.Message)
			}
		}
	}()
	return true
}

func (s *Scheduler) codeArtsStatus() CodeArtsDailyState {
	now := s.now()
	cfg := s.Settings.CodeArtsDaily()
	state := CodeArtsDailyState{
		CodeArtsDailyConfig: cfg, Available: s.Channel != nil, Running: s.daily.running.Load(),
		States: make(map[string]int),
	}
	s.mu.Lock()
	last := s.daily.lastRun
	s.mu.Unlock()
	if !last.IsZero() {
		state.LastRun = last.In(beijing).Format(time.RFC3339)
	}
	start := dailyStart(now, cfg.Time)
	next := start
	if !now.Before(start) {
		next = start.AddDate(0, 0, 1)
	}
	for _, acc := range s.codeArtsAccounts(cfg) {
		state.AccountCount++
		result := codeartsync.DailyStatus(acc.Record(), now)
		key := result.State
		if key == "" {
			key = "not_run"
			if acc.Record().CodeArtsNeedsLogin() {
				key = codeartsync.DailyNeedsLogin
			}
		}
		state.States[key]++
		if !state.Available || now.Before(start) || !s.codeArtsTaskAllowed(acc, now) {
			continue
		}
		if s.Channel.CanCheckin(acc) {
			next = now
		} else if result.NextRetry > now.Unix() {
			if retry := time.Unix(result.NextRetry, 0); retry.Before(next) {
				next = retry
			}
		}
	}
	if state.Available && s.Settings.SchedulerEnabled() && cfg.Enabled {
		state.NextRun = next.In(beijing).Format(time.RFC3339)
	}
	return state
}
