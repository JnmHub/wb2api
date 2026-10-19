// Package scheduler runs the unattended account chores: keeping tokens alive,
// the CN daily check-in and activity report, the INTL daily chat, and the slow
// background refresh of credit balances.
//
// Everything is spread out on purpose — a random pause between accounts, one
// balance refresh per tick — because bursts of account-service calls from one
// machine are exactly what upstream risk control looks for.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"wb2api/internal/ops"
	"wb2api/internal/pool"
	"wb2api/internal/quota"
	"wb2api/internal/realm"
	"wb2api/internal/store/settings"
)

// Tokens refreshes credentials.
type Tokens interface {
	Refresh(ctx context.Context, a *pool.Account) error
}

// Scheduler is the background job runner.
type Scheduler struct {
	Pool      *pool.Pool
	Ops       *ops.Client
	Refresher Tokens
	Settings  *settings.Store
	Log       *slog.Logger
	// Hold returns how long a realm is paused after a risk-control wave (nil = never).
	Hold func(realm string) time.Duration
	// Windows is the model-quota window probe (nil = disabled).
	Windows *quota.Windows
	// Scan is the active quota scan (nil = disabled).
	Scan *quota.Scanner
	// Stagger is the pause between accounts (default 2–5 s random).
	Stagger func() time.Duration
	// Pause replaces the one-second spacing between one account's chores (tests).
	Pause func(time.Duration)
	Now   func() time.Time

	mu       sync.Mutex
	running  bool
	lastRun  time.Time
	lastNote string
	logs     []string
	parked   map[string]time.Time // accounts whose balance refresh failed, until when
	credits  CreditsRefreshState
	probing  atomic.Bool
	scanning atomic.Bool
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scheduler) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Scheduler) stagger() time.Duration {
	if s.Stagger != nil {
		return s.Stagger()
	}
	return 2*time.Second + time.Duration(rand.Int63n(int64(3*time.Second)))
}

func (s *Scheduler) pause(d time.Duration) {
	if s.Pause != nil {
		s.Pause(d)
		return
	}
	if d > 0 {
		time.Sleep(d)
	}
}

func hoursInclude(hours []int, h int) bool {
	for _, x := range hours {
		if x == h {
			return true
		}
	}
	return false
}

func (s *Scheduler) note(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	s.logger().Info("[调度器] "+msg, "tag", "scheduler")
	s.mu.Lock()
	s.logs = append(s.logs, s.now().Format("2006-01-02 15:04:05")+" "+msg)
	if len(s.logs) > 60 {
		s.logs = s.logs[len(s.logs)-60:]
	}
	s.mu.Unlock()
}

// Hours returns the hours the cycle fires at (union of every configured grid).
func (s *Scheduler) hours() []int {
	h := s.Settings.ScheduleHours()
	seen := map[int]bool{}
	var out []int
	for _, list := range h {
		for _, x := range list {
			if !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
	}
	sort.Ints(out)
	return out
}

// NextFire is the next scheduled run.
func (s *Scheduler) NextFire() time.Time {
	now := s.now()
	hours := s.hours()
	if len(hours) == 0 {
		return time.Time{}
	}
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if t.After(now) {
			return t
		}
	}
	tom := now.AddDate(0, 0, 1)
	return time.Date(tom.Year(), tom.Month(), tom.Day(), hours[0], 0, 0, 0, now.Location())
}

// Trigger runs a cycle now in the background; false when one is already running.
func (s *Scheduler) Trigger() bool {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return false
	}
	s.mu.Unlock()
	go s.Cycle(context.Background(), "手动立即触发")
	return true
}

// Cycle performs one pass over every account.
func (s *Scheduler) Cycle(ctx context.Context, reason string) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		s.note("跳过本次巡检 (%s)：上一轮仍在执行", reason)
		return
	}
	s.running, s.lastRun = true, s.now()
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.running = false; s.mu.Unlock() }()

	s.note("开始执行任务 (%s)...", reason)
	hour := s.now().Hour()
	sched := s.Settings.ScheduleHours()
	travelHours := hoursInclude(sched["travel"], hour)
	nightHours := hoursInclude(sched["cat"], hour)
	accs := append([]*pool.Account(nil), s.Pool.All()...)
	if len(accs) == 0 {
		s.note("暂无可用的活跃账号，跳过本次巡检")
		return
	}
	var kept, checkin, report, chat, travel int
	for i, acc := range accs {
		if ctx.Err() != nil {
			return
		}
		if i > 0 {
			if d := s.stagger(); d > 0 {
				select {
				case <-time.After(d):
				case <-ctx.Done():
					return
				}
			}
		}
		rec := acc.Record()
		uid8 := short(rec.UID)
		// An account the upstream restricted is left alone: probing it from an
		// unattended job only drags the other accounts on the same device in.
		if until := acc.RestrictedUntil(); until.After(s.now()) {
			s.note("跳过受限账号 [%s]：上游风控限制剩余 %ds", uid8, int(until.Sub(s.now()).Seconds()))
			continue
		}
		if s.Hold != nil {
			if rem := s.Hold(rec.Realm); rem > 0 {
				s.note("跳过 [%s]：%s 区域风控暂停剩余 %ds", uid8, rec.Realm, int(rem.Seconds()))
				continue
			}
		}
		if !acc.Enabled() || rec.AccessToken == "" {
			continue
		}
		if exp := acc.ExpiresAt(); exp != 0 && time.Until(time.Unix(exp, 0)) < 2*time.Hour && s.Refresher != nil {
			s.note("账号 [%s] Token 即将到期，执行主动保活刷新...", uid8)
			if err := s.Refresher.Refresh(ctx, acc); err == nil {
				kept++
			} else {
				s.note("! 账号 [%s] Token 保活刷新失败: %v", uid8, err)
			}
		}
		switch rec.Realm {
		case realm.CN:
			if s.Ops.CanCheckin(acc) {
				res := s.Ops.Checkin(ctx, acc, "scheduler")
				if res.OK {
					checkin++
					s.note("✓ 账号 [%s] 自动签到成功: %s", uid8, res.Msg)
				} else {
					s.note("! 账号 [%s] 自动签到未成功: %s", uid8, firstNonEmpty(res.Error, res.Msg))
				}
				s.pause(time.Second)
			}
			if s.Ops.CanReportActivity(acc) {
				res := s.Ops.ReportActivity(ctx, acc, "scheduler")
				if res.OK {
					report++
					s.note("✓ 账号 [%s] %s", uid8, res.Msg)
				} else {
					s.note("! 账号 [%s] 对话活跃上报失败: %s", uid8, firstNonEmpty(res.Error, res.Msg))
				}
				s.pause(time.Second)
			}
			if travelHours {
				res := s.Ops.CatTravel(ctx, acc)
				if res.OK {
					travel++
					s.note("✓ 账号 [%s] 猫猫日常: %s", uid8, res.Msg)
				} else {
					s.note("! 账号 [%s] 猫猫日常失败: %s", uid8, firstNonEmpty(res.Error, res.Msg))
				}
				s.pause(time.Second)
			}
			if res := s.Ops.RunStreakBonus(ctx, acc); res.OK {
				s.note("账号 [%s] 连登管家: %s", uid8, res.Msg)
			} else {
				s.note("! 账号 [%s] 连登管家失败: %s", uid8, firstNonEmpty(res.Error, res.Msg))
			}
			s.pause(time.Second)
			if nightHours {
				res := s.Ops.NightGrowth(ctx, acc)
				if res.OK {
					s.note("账号 [%s] 夜猫子: %s", uid8, res.Msg)
				} else {
					s.note("! 账号 [%s] 夜猫子失败: %s", uid8, firstNonEmpty(res.Error, res.Msg))
				}
			}
		case realm.Intl:
			if s.Ops.CanDailyChat(acc) {
				res := s.Ops.DailyChat(ctx, acc, "scheduler")
				if res.OK {
					chat++
					s.note("✓ 账号 [%s] 每日活跃对话成功: %s", uid8, res.Msg)
				} else {
					s.note("! 账号 [%s] 每日活跃对话失败: %s", uid8, firstNonEmpty(res.Error, res.Msg))
				}
			}
		}
	}
	s.mu.Lock()
	s.lastNote = fmt.Sprintf("Token保活 %d，国内签到 %d，活跃上报 %d，猫猫日常 %d，国际活跃 %d", kept, checkin, report, travel, chat)
	s.mu.Unlock()
	s.note("巡检完成：%s", s.lastNote)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func short(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// runWindows starts one window-probe pass in the background. Probes are
// independent of the unattended cycle: they fire on the upstream's clock even
// when the scheduler is switched off, and must not wait behind a long cycle.
func (s *Scheduler) runWindows(ctx context.Context) {
	if s.Windows == nil || s.Windows.Prober == nil {
		return
	}
	if !s.probing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.probing.Store(false)
		s.Windows.RunDue(ctx, s.Pool, quota.BatchLimit)
	}()
}

// runScans starts one active quota-scan pass in the background, independent of
// the unattended cycle for the same reason as the window probe.
func (s *Scheduler) runScans(ctx context.Context) {
	if s.Scan == nil {
		return
	}
	if !s.scanning.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.scanning.Store(false)
		s.Scan.RunDue(ctx, s.Pool, false)
	}()
}

// Run blocks until ctx ends: after a short settle time it does one start-up
// pass, then fires the cycle on the configured hours. Window probes run on
// their own clock every tick, whether or not the unattended cycle is enabled.
func (s *Scheduler) Run(ctx context.Context) {
	select {
	case <-time.After(10 * time.Second):
	case <-ctx.Done():
		return
	}
	s.runWindows(ctx)
	s.runScans(ctx)
	if s.Settings.SchedulerEnabled() {
		s.Cycle(ctx, "启动初次初始化巡检")
	}
	lastFired := time.Time{}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.runWindows(ctx)
		s.runScans(ctx)
		now := s.now()
		if !s.Settings.SchedulerEnabled() || now.Minute() != 0 || now.Sub(lastFired) < 2*time.Minute {
			continue
		}
		for _, h := range s.hours() {
			if h == now.Hour() {
				lastFired = now
				s.Cycle(ctx, fmt.Sprintf("整点排程命中 (%d:00)", h))
				break
			}
		}
	}
}

// Status describes the scheduler for the panel.
type Status struct {
	Enabled  bool                `json:"enabled"`
	Running  bool                `json:"running"`
	LastRun  string              `json:"last_run_time"`
	NextRun  string              `json:"next_run_time"`
	LastNote string              `json:"last_summary"`
	Schedule map[string][]int    `json:"schedule"`
	Logs     []string            `json:"logs"`
	Credits  CreditsRefreshState `json:"credits"`
}

// CreditsRefreshState is the background balance refresher's progress.
type CreditsRefreshState struct {
	TTLHours float64 `json:"ttl_hours"`
	LastRun  string  `json:"last_run"`
	LastUID  string  `json:"last_uid"`
	LastErr  string  `json:"last_error"`
}

// Status returns the current state.
func (s *Scheduler) Status() Status {
	next := s.NextFire()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{Enabled: s.Settings.SchedulerEnabled(), Running: s.running, LastNote: s.lastNote,
		Schedule: s.Settings.ScheduleHours(), Logs: append([]string(nil), s.logs...)}
	st.LastRun, st.NextRun = "尚未运行", "待调度"
	if !s.lastRun.IsZero() {
		st.LastRun = s.lastRun.Format("2006-01-02 15:04:05")
	}
	if !next.IsZero() {
		st.NextRun = next.Format("2006-01-02 15:04:05")
	}
	st.Credits = s.credits
	st.Credits.TTLHours = s.Settings.CreditsRefreshHours()
	return st
}
