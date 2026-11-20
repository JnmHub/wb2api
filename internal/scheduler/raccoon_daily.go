package scheduler

import (
	"context"
	"slices"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
	"wb2api/internal/store/settings"
)

// The raccoon (小浣熊) daily task is the "desktop home" points grant, which is a
// per-day idempotent side effect upstream. It therefore has its own schedule,
// its own enable switch and its own status block: sharing the WorkBuddy grid
// would tie one vendor's outage to the other's.

type raccoonRun struct {
	ctx     context.Context
	done    chan struct{}
	lastRun time.Time
	retry   map[string]time.Time
}

const raccoonCheckinRetry = 5 * time.Minute

// RaccoonDailyState describes the raccoon schedule for the panel.
type RaccoonDailyState struct {
	settings.RaccoonDailyConfig
	Available    bool           `json:"available"`
	Running      bool           `json:"running"`
	LastRun      string         `json:"last_run_at,omitempty"`
	NextRun      string         `json:"next_run_at,omitempty"`
	AccountCount int            `json:"account_count"`
	States       map[string]int `json:"states"`
}

// raccoonAccounts filters the pool down to the accounts this run covers.
func (s *Scheduler) raccoonAccounts(cfg settings.RaccoonDailyConfig) []*pool.Account {
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
		if realm.IsRaccoon(rec.Realm) && rec.IsEnabled() && rec.TasksOn() &&
			(len(uidSet) == 0 || uidSet[acc.UID]) {
			list = append(list, acc)
		}
	}
	return list
}

func (s *Scheduler) raccoonTaskAllowed(acc *pool.Account, now time.Time) bool {
	return !acc.RestrictedUntil().After(now) && (s.Hold == nil || s.Hold(acc.Realm) <= 0)
}

// TriggerRaccoon queues a manual pass with the scheduler's lifetime.
func (s *Scheduler) TriggerRaccoon() bool {
	s.mu.Lock()
	ctx := s.raccoon.ctx
	s.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	return s.runRaccoon(ctx, true)
}

func (s *Scheduler) raccoonState(acc *pool.Account, now time.Time) string {
	if checkedInToday(acc.Record(), now) {
		return "done"
	}
	if !s.Raccoon.CanCheckin(acc) {
		return "skip"
	}
	if !s.raccoonTaskAllowed(acc, now) {
		return "hold"
	}
	s.mu.Lock()
	retrying := s.raccoon.retry[acc.UID].After(now)
	s.mu.Unlock()
	if retrying {
		return "retry"
	}
	return "due"
}

// runRaccoon queues one pass without blocking the WorkBuddy scheduler.
func (s *Scheduler) runRaccoon(ctx context.Context, manual bool) bool {
	if s.Raccoon == nil || s.Settings == nil || ctx.Err() != nil {
		return false
	}
	cfg := s.Settings.RaccoonDaily()
	now := s.now()
	start := dailyStart(now, cfg.Time)
	if !manual && (!s.isLeader() || !s.Settings.SchedulerEnabled() || !cfg.Enabled || now.Before(start)) {
		return false
	}
	list := slices.DeleteFunc(s.raccoonAccounts(cfg), func(acc *pool.Account) bool {
		if manual {
			return !s.raccoonTaskAllowed(acc, now) || !s.Raccoon.CanCheckin(acc)
		}
		return s.raccoonState(acc, now) != "due"
	})
	if len(list) == 0 {
		return false
	}
	s.mu.Lock()
	if s.raccoon.done != nil || ctx.Err() != nil {
		s.mu.Unlock()
		return false
	}
	done := make(chan struct{})
	s.raccoon.done = done
	s.raccoon.lastRun = now
	for uid, retry := range s.raccoon.retry {
		if !retry.After(now) {
			delete(s.raccoon.retry, uid)
		}
	}
	s.mu.Unlock()
	local := now.In(beijing)
	midnight := time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, beijing)
	go func() {
		defer func() {
			s.mu.Lock()
			close(done)
			s.raccoon.done = nil
			s.mu.Unlock()
		}()
		s.checkinRaccoonAccounts(ctx, manual, midnight, list)
	}()
	return true
}

func (s *Scheduler) checkinRaccoonAccounts(ctx context.Context, manual bool, midnight time.Time, list []*pool.Account) {
	reason := "定时"
	if manual {
		reason = "手动"
	}
	s.note("小浣熊签到开始（%s，%d 个账号）", reason, len(list))
	var ok, skipped, failed int
	attempted := false
	for _, acc := range list {
		if attempted {
			timer := time.NewTimer(s.stagger())
			select {
			case <-ctx.Done():
			case <-timer.C:
			}
			timer.Stop()
			attempted = false
		}
		now := s.now()
		if ctx.Err() != nil || !now.Before(midnight) {
			break
		}
		current := s.Settings.RaccoonDaily()
		if !manual && (!s.isLeader() || !s.Settings.SchedulerEnabled() || !current.Enabled || now.Before(dailyStart(now, current.Time))) {
			break
		}
		if s.Pool.Get(acc.UID) != acc || len(current.UIDs) > 0 && !slices.Contains(current.UIDs, acc.UID) ||
			!s.raccoonTaskAllowed(acc, now) || !s.Raccoon.CanCheckin(acc) || !manual && s.raccoonState(acc, now) != "due" {
			skipped++
			continue
		}
		attempted = true
		result, err := s.Raccoon.Checkin(ctx, acc)
		s.mu.Lock()
		if err == nil && result.Success {
			delete(s.raccoon.retry, acc.UID)
		} else if ctx.Err() == nil {
			if s.raccoon.retry == nil {
				s.raccoon.retry = make(map[string]time.Time)
			}
			s.raccoon.retry[acc.UID] = s.now().Add(raccoonCheckinRetry)
		}
		s.mu.Unlock()
		switch {
		case err != nil:
			failed++
			s.note("小浣熊签到失败 %s: %v", short(acc.UID), err)
		case result.Success:
			ok++
			s.note("小浣熊签到成功 %s: %s", short(acc.UID), result.Message)
		default:
			// "no grant today" is not a failure: the ledger simply has no entry.
			skipped++
			s.note("小浣熊签到无入账 %s: %s", short(acc.UID), result.Message)
		}
	}
	s.note("小浣熊签到结束：成功 %d，跳过 %d，失败 %d", ok, skipped, failed)
}

// raccoonDailyStatus renders the panel block.
func (s *Scheduler) raccoonDailyStatus() RaccoonDailyState {
	if s.Settings == nil {
		return RaccoonDailyState{}
	}
	cfg := s.Settings.RaccoonDaily()
	out := RaccoonDailyState{RaccoonDailyConfig: cfg, States: map[string]int{}}
	out.Available = s.Raccoon != nil
	if !out.Available {
		return out
	}
	list := s.raccoonAccounts(cfg)
	out.AccountCount = len(list)
	now := s.now()
	for _, acc := range list {
		out.States[s.raccoonState(acc, now)]++
	}
	s.mu.Lock()
	last := s.raccoon.lastRun
	running := s.raccoon.done != nil
	s.mu.Unlock()
	out.Running = running
	if !last.IsZero() {
		out.LastRun = last.In(beijing).Format("2006-01-02 15:04:05")
	}
	next := dailyStart(now, cfg.Time)
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	if s.Settings.SchedulerEnabled() && cfg.Enabled {
		out.NextRun = next.In(beijing).Format("2006-01-02 15:04:05")
	}
	return out
}

// workBuddyScope reports whether an account belongs to the daily-chore sweep.
// Every other channel has its own schedule; including one here would earn a 401
// per chore and, worse, write its failure into the WorkBuddy activity log.
func workBuddyScope(acc *pool.Account) bool {
	if acc == nil {
		return false
	}
	return realm.Family(acc.Realm) == "workbuddy"
}

// checkedInToday reports a successful check-in already recorded for the Beijing
// calendar day. A missing or unparseable stamp counts as "not yet".
func checkedInToday(rec *accounts.Record, now time.Time) bool {
	if rec == nil {
		return false
	}
	stamp, ok := rec.LastCheckin.(map[string]any)
	if !ok {
		return false
	}
	if done, _ := stamp["ok"].(bool); !done {
		return false
	}
	at, _ := stamp["at"].(string)
	if len(at) < 10 {
		return false
	}
	return at[:10] == now.In(beijing).Format("2006-01-02")
}
