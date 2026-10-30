package pool

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
)

// Account-level failure governance (port of the legacy Account.note_* family).
// A repeatedly soft-limited credential backs off exponentially instead of being
// retried on every request; a credential that keeps failing hard trips a
// breaker; unknown failures degrade it for a while. Counters only count as
// "consecutive" inside FailWindow.
const (
	SoftRateBase           = 600 * time.Second  // first verified account-level 429
	SoftRateMax            = 7200 * time.Second // doubling up to 2 hours
	SoftRateUnverifiedBase = 60 * time.Second   // unscoped 429: a window on one model
	SoftRateUnverifiedMax  = 600 * time.Second
	BreakerThreshold       = 3
	BreakerCooldown        = 1800 * time.Second
	BreakerCooldownMax     = 21600 * time.Second
	DegradeThreshold       = 5
	DegradeCooldown        = 600 * time.Second
	DegradeCooldownMax     = 7200 * time.Second
	// RestrictedAccountCooldown is the first (and persisted) window of an
	// upstream risk-control restriction (403 / 11140).
	RestrictedAccountCooldown = 7200 * time.Second
)

// FailWindow: two failures further apart than this do not count as
// consecutive (WB_FAIL_WINDOW seconds; 0 disables the decay).
var FailWindow = func() time.Duration {
	if v := strings.TrimSpace(os.Getenv("WB_FAIL_WINDOW")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return time.Duration(f * float64(time.Second))
		}
	}
	return 600 * time.Second
}()

func exponentialBackoff(count int, base, max time.Duration, offset int) time.Duration {
	step := count - offset
	if step < 0 {
		step = 0
	}
	if step > 20 {
		step = 20
	}
	d := time.Duration(float64(base) * math.Pow(2, float64(step)))
	if d > max {
		d = max
	}
	return d
}

// SoftBackoff is the cooldown for streak consecutive account-level soft limits.
func SoftBackoff(streak int) time.Duration {
	return exponentialBackoff(streak, SoftRateBase, SoftRateMax, 1)
}

// UnverifiedSoftBackoff is the model-scoped window for unscoped 429s.
func UnverifiedSoftBackoff(streak int) time.Duration {
	return exponentialBackoff(streak, SoftRateUnverifiedBase, SoftRateUnverifiedMax, 1)
}

// BreakerBackoff is the breaker window after fails consecutive hard failures.
func BreakerBackoff(fails int) time.Duration {
	return exponentialBackoff(fails, BreakerCooldown, BreakerCooldownMax, BreakerThreshold)
}

// DegradeBackoff is the degrade window after fails consecutive unknown failures.
func DegradeBackoff(fails int) time.Duration {
	return exponentialBackoff(fails, DegradeCooldown, DegradeCooldownMax, DegradeThreshold)
}

// NextLocal4AM is the next local 04:00, the daily reset an out-of-credits park
// waits for.
func NextLocal4AM(now time.Time) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, now.Location())
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// governState is the mutable failure bookkeeping (guarded by Account.mu).
type governState struct {
	softStreak, unscopedStreak int
	fails, degradeCount        int
	lastFailAt, lastSoftAt     time.Time
	lastError, lastErrorDetail string
	coolAt                     map[string]time.Time // when each model cooldown started
	caps                       map[string]Cap       // upstream-declared quota windows
}

// Cap is an upstream "window quota used up" verdict (code 6004) for one model.
type Cap struct {
	At    time.Time
	Reset time.Time
}

func (a *Account) setErr(msg, detail string) {
	a.g.lastError = truncRunes(msg, 200)
	a.g.lastErrorDetail = truncRunes(detail, 2000)
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// LastError is the most recent failure note (empty after a success).
func (a *Account) LastError() (msg, detail string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.g.lastError, a.g.lastErrorDetail
}

// Streaks reports the failure counters (for the panel and diagnostics).
func (a *Account) Streaks() (soft, unscoped, fails, degrade int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.g.softStreak, a.g.unscopedStreak, a.g.fails, a.g.degradeCount
}

func (a *Account) decaySoftLocked(now time.Time) {
	if FailWindow > 0 && !a.g.lastSoftAt.IsZero() && now.Sub(a.g.lastSoftAt) > FailWindow {
		a.g.softStreak, a.g.unscopedStreak = 0, 0
	}
	a.g.lastSoftAt = now
}

func (a *Account) decayStrikesLocked(now time.Time) {
	if FailWindow > 0 && !a.g.lastFailAt.IsZero() && now.Sub(a.g.lastFailAt) > FailWindow {
		a.g.fails, a.g.degradeCount = 0, 0
	}
	a.g.lastFailAt = now
}

func storeMax(v *atomicInt64, ns int64) {
	for {
		cur := v.Load()
		if cur >= ns || v.CompareAndSwap(cur, ns) {
			return
		}
	}
}

// NoteSoftRate records an account-level 429: a cooldown that doubles from 10
// minutes to 2 hours. It returns the cooldown applied.
func (a *Account) NoteSoftRate(msg string, now time.Time) time.Duration {
	a.mu.Lock()
	a.decaySoftLocked(now)
	a.g.softStreak++
	wait := SoftBackoff(a.g.softStreak)
	a.setErr(msg, "")
	a.mu.Unlock()
	storeMax(&a.cooldownTo, now.Add(wait).UnixNano())
	a.touch()
	return wait
}

// NoteUnscopedRate grows the model-scoped ladder (60 s up to 10 min) and returns
// the window for this model. The count is separate from the account ladder so
// the two never feed each other.
func (a *Account) NoteUnscopedRate(now time.Time) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.decaySoftLocked(now)
	a.g.unscopedStreak++
	return UnverifiedSoftBackoff(a.g.unscopedStreak)
}

// UnscopedStreak is the current unscoped 429 count.
func (a *Account) UnscopedStreak() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.g.unscopedStreak
}

// NoteModelError throttles one model on this account until now+wait (or until
// `until` when the body named a reset clock), leaving its other models usable.
// The cooldown is written to the record so a restart does not forget it: the
// record to persist is returned when the window was extended.
func (a *Account) NoteModelError(model string, until time.Time, wait time.Duration, msg, detail string, now time.Time) (*accounts.Record, bool) {
	end := now.Add(wait)
	if !until.IsZero() {
		end = until
		if end.Sub(now) < time.Second {
			end = now.Add(time.Second)
		}
	}
	a.mu.Lock()
	a.setErr(msg, detail)
	if a.modelCool == nil {
		a.modelCool = map[string]int64{}
	}
	if a.g.coolAt == nil {
		a.g.coolAt = map[string]time.Time{}
	}
	extended := a.modelCool[model] < end.UnixNano()
	a.modelCool[model] = end.UnixNano()
	a.setCoolWhyLocked(model, "soft")
	a.g.coolAt[model] = now
	a.mu.Unlock()
	a.touch()
	if !extended {
		return nil, false
	}
	return a.persistCool(), true
}

// BlockModel marks one model unusable on this account until a deadline without
// touching the account's error counters: 11158 says the model is not part of
// this account's entitlement, not that the account is failing.
func (a *Account) BlockModel(model string, until, now time.Time) (*accounts.Record, bool) {
	if model == "" || !until.After(now) {
		return nil, false
	}
	a.mu.Lock()
	if a.modelCool == nil {
		a.modelCool = map[string]int64{}
	}
	extended := a.modelCool[model] < until.UnixNano()
	if extended {
		a.modelCool[model] = until.UnixNano()
		a.setCoolWhyLocked(model, "entitlement")
	}
	a.mu.Unlock()
	if !extended {
		return nil, false
	}
	a.touch()
	return a.persistCool(), true
}

// persistCool snapshots the live model cooldowns into a record copy.
func (a *Account) persistCool() *accounts.Record {
	now := time.Now()
	a.mu.Lock()
	cool := make(map[string]float64, len(a.modelCool))
	why := make(map[string]string, len(a.modelWhy))
	for m, ns := range a.modelCool {
		if ns > now.UnixNano() {
			cool[m] = float64(ns) / 1e9
			if w := a.modelWhy[m]; w != "" {
				why[m] = w
			}
		}
	}
	a.mu.Unlock()
	next := *a.Record()
	next.ModelCool = cool
	next.ModelCoolWhy = why
	a.rec.Store(&next)
	a.touch()
	return &next
}

// NoteFailure records a 5xx / transport failure toward the breaker.
func (a *Account) NoteFailure(msg string, now time.Time) {
	a.mu.Lock()
	a.setErr(msg, "")
	a.decayStrikesLocked(now)
	a.g.fails++
	var until time.Time
	if a.g.fails >= BreakerThreshold {
		until = now.Add(BreakerBackoff(a.g.fails))
	}
	a.mu.Unlock()
	if !until.IsZero() {
		storeMax(&a.breakerTo, until.UnixNano())
	}
	a.touch()
}

// NoteUnknownFailure feeds the degrade counter and the shared breaker counter.
func (a *Account) NoteUnknownFailure(msg string, now time.Time) {
	a.mu.Lock()
	a.setErr(msg, "")
	a.decayStrikesLocked(now)
	a.g.degradeCount++
	a.g.fails++
	var deg, brk time.Time
	if a.g.degradeCount >= DegradeThreshold {
		deg = now.Add(DegradeBackoff(a.g.degradeCount))
	}
	if a.g.fails >= BreakerThreshold {
		brk = now.Add(BreakerBackoff(a.g.fails))
	}
	a.mu.Unlock()
	if !deg.IsZero() {
		storeMax(&a.degradeTo, deg.UnixNano())
	}
	if !brk.IsZero() {
		storeMax(&a.breakerTo, brk.UnixNano())
	}
	a.touch()
}

// NoteBalanceCooled parks the account (402 / out of credits) until the next
// local 04:00, when the daily reset puts credits back.
func (a *Account) NoteBalanceCooled(msg string, now time.Time) time.Time {
	until := NextLocal4AM(now)
	a.mu.Lock()
	a.setErr(msg, "")
	a.mu.Unlock()
	storeMax(&a.balanceTo, until.UnixNano())
	a.touch()
	return until
}

// ReviveBalanceCooldown lifts a 402 park when a credits refresh shows a balance
// again (nothing else is cleared).
func (a *Account) ReviveBalanceCooldown() bool {
	if a.balanceTo.Load() == 0 {
		return false
	}
	remain, ok := toNumber(a.Record().Credits["remain"])
	if !ok || remain <= 0 {
		return false
	}
	a.balanceTo.Store(0)
	a.mu.Lock()
	a.g.lastError, a.g.lastErrorDetail = "", ""
	a.mu.Unlock()
	a.touch()
	return true
}

func toNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case interface{ Float64() (float64, error) }:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}

// NoteSuccess clears every account-level penalty after a served request —
// including a persisted upstream restriction, since a success is the one piece
// of evidence that the vendor lifted it. It returns the record to persist when
// a restriction was lifted.
func (a *Account) NoteSuccess(model string) *accounts.Record {
	a.mu.Lock()
	if model != "" {
		delete(a.modelCool, model)
		delete(a.g.coolAt, model)
	}
	a.g.softStreak, a.g.unscopedStreak, a.g.fails, a.g.degradeCount = 0, 0, 0, 0
	a.g.lastFailAt, a.g.lastSoftAt = time.Time{}, time.Time{}
	a.g.lastError, a.g.lastErrorDetail = "", ""
	a.mu.Unlock()
	a.breakerTo.Store(0)
	a.degradeTo.Store(0)
	a.cooldownTo.Store(0)
	var rec *accounts.Record
	if a.restrictedTo.Swap(0) != 0 {
		next := *a.Record()
		next.RestrictedUntil = 0
		a.rec.Store(&next)
		rec = &next
	}
	return rec
}

// ThrottleWait is how long until the account can serve model again (0 = now).
func (a *Account) ThrottleWait(model string, now time.Time) time.Duration {
	if !a.enabled.Load() || a.Record().AccessToken == "" {
		return 0
	}
	ns := now.UnixNano()
	best := int64(0)
	for _, v := range []int64{a.cooldownTo.Load(), a.balanceTo.Load(), a.breakerTo.Load(), a.degradeTo.Load(), a.restrictedTo.Load()} {
		if v-ns > best {
			best = v - ns
		}
	}
	if model != "" {
		a.mu.Lock()
		if v := a.modelCool[model]; v-ns > best {
			best = v - ns
		}
		a.mu.Unlock()
	}
	return time.Duration(best)
}

// ClearError lifts local throttle windows. keepCaps keeps upstream-declared
// quota windows (6004), which are facts rather than local guesses; an upstream
// restriction is never cleared here.
func (a *Account) ClearError(model string, keepCaps bool, now time.Time) {
	a.mu.Lock()
	switch {
	case model != "":
		if !(keepCaps && a.capUntilLocked(model, now).After(now)) {
			delete(a.modelCool, model)
			delete(a.modelWhy, model)
			delete(a.g.coolAt, model)
		}
	case keepCaps:
		for m := range a.modelCool {
			if !a.capUntilLocked(m, now).After(now) {
				delete(a.modelCool, m)
				delete(a.modelWhy, m)
				delete(a.g.coolAt, m)
			}
		}
	default:
		a.modelCool, a.modelWhy, a.g.coolAt = nil, nil, nil
	}
	a.g.lastError, a.g.lastErrorDetail = "", ""
	a.mu.Unlock()
	a.cooldownTo.Store(0)
	a.balanceTo.Store(0)
	a.touch()
}

// ClearModelCooldown lifts one model's throttle, leaving account state alone.
func (a *Account) ClearModelCooldown(model string) {
	if model == "" {
		return
	}
	a.mu.Lock()
	delete(a.modelCool, model)
	delete(a.modelWhy, model)
	a.mu.Unlock()
	a.touch()
}

func (a *Account) capUntilLocked(model string, now time.Time) time.Time {
	if c, ok := a.g.caps[model]; ok && c.Reset.After(now) {
		return c.Reset
	}
	return time.Time{}
}

// CapUntil is the end of the model's upstream-declared quota window (zero when
// not in one).
func (a *Account) CapUntil(model string, now time.Time) time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.capUntilLocked(model, now)
}

// CappedModels lists the models still inside an upstream quota window.
func (a *Account) CappedModels(now time.Time) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for m := range a.g.caps {
		if a.capUntilLocked(m, now).After(now) {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// NoteModelCap records "this model's window quota is used up" (429 / 6004) and
// persists the reset clock, which holds across restarts. It only applies to the
// free models the cap linkage covers; the record to persist is returned when
// the cap is new (a repeat hit on the same window rewrites nothing).
func (a *Account) NoteModelCap(model string, reset, now time.Time) (*accounts.Record, bool) {
	if model == "" || reset.IsZero() || !realm.IsFreeCapModel(a.Realm, model) {
		return nil, false
	}
	a.mu.Lock()
	if old, ok := a.g.caps[model]; ok && old.Reset.Equal(reset) {
		// same window hit again: no rewrite, but make sure the cooldown stays
		if a.modelCool[model] < reset.UnixNano() {
			a.modelCool[model] = reset.UnixNano()
		}
		a.setCoolWhyLocked(model, "cap")
		a.mu.Unlock()
		return nil, false
	}
	if a.g.caps == nil {
		a.g.caps = map[string]Cap{}
	}
	a.g.caps[model] = Cap{At: now, Reset: reset}
	if a.modelCool == nil {
		a.modelCool = map[string]int64{}
	}
	if a.modelCool[model] < reset.UnixNano() {
		a.modelCool[model] = reset.UnixNano()
	}
	a.setCoolWhyLocked(model, "cap")
	a.mu.Unlock()
	return a.persistCaps(), true
}

func (a *Account) persistCaps() *accounts.Record {
	a.mu.Lock()
	caps := make(map[string]accounts.ModelCap, len(a.g.caps))
	for m, c := range a.g.caps {
		caps[m] = accounts.ModelCap{At: float64(c.At.UnixNano()) / 1e9, Reset: float64(c.Reset.UnixNano()) / 1e9}
	}
	a.mu.Unlock()
	next := *a.Record()
	next.ModelCaps = caps
	a.rec.Store(&next)
	a.touch()
	return &next
}

// restoreCaps loads persisted quota windows and re-arms the model cooldowns
// still running (called by SetRecord).
func (a *Account) restoreCaps(rec *accounts.Record, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.g.caps = nil
	for m, until := range rec.ModelCool {
		if until <= 0 {
			continue
		}
		end := time.Unix(0, int64(until*1e9))
		if !end.After(now) {
			continue
		}
		if a.modelCool == nil {
			a.modelCool = map[string]int64{}
		}
		if a.modelCool[m] < end.UnixNano() {
			a.modelCool[m] = end.UnixNano()
		}
		if w := rec.ModelCoolWhy[m]; w != "" {
			a.setCoolWhyLocked(m, w)
		}
	}
	for m, c := range rec.ModelCaps {
		if !realm.IsFreeCapModel(a.Realm, m) || c.Reset <= 0 {
			continue
		}
		reset := time.Unix(0, int64(c.Reset*1e9))
		if a.g.caps == nil {
			a.g.caps = map[string]Cap{}
		}
		a.g.caps[m] = Cap{At: time.Unix(0, int64(c.At*1e9)), Reset: reset}
		if reset.After(now) {
			if a.modelCool == nil {
				a.modelCool = map[string]int64{}
			}
			if a.modelCool[m] < reset.UnixNano() {
				a.modelCool[m] = reset.UnixNano()
			}
			if a.modelWhy[m] == "" {
				a.setCoolWhyLocked(m, "cap")
			}
		}
	}
}

// ModelCooldownStart is when the current cooldown of model began (zero if none).
func (a *Account) ModelCooldownStart(model string) time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.g.coolAt[model]
}

func humanLeft(d time.Duration) string {
	secs := int(math.Round(d.Seconds()))
	if secs < 0 {
		secs = 0
	}
	switch {
	case secs < 60:
		return fmt.Sprintf("%ds", secs)
	case secs < 3600:
		m, s := secs/60, secs%60
		if s != 0 {
			return fmt.Sprintf("%dm%02ds", m, s)
		}
		return fmt.Sprintf("%dm", m)
	case secs < 86400:
		h, m := secs/3600, secs%3600/60
		if m != 0 {
			return fmt.Sprintf("%dh%02dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%.1f days", float64(secs)/86400)
}

// UnavailableReason says why the account cannot serve model right now ("" when
// it can). The order matches Ready, so the reason is the one that refused it.
func (a *Account) UnavailableReason(model string, now time.Time) string {
	if !a.enabled.Load() {
		return "已停用"
	}
	if a.Record().AccessToken == "" {
		return "无凭证"
	}
	if a.tokenBad.Load() {
		return "凭证已过期"
	}
	a.mu.Lock()
	disabled := model != "" && a.disabledMd[model]
	a.mu.Unlock()
	if disabled {
		return "该模型已被人工禁用"
	}
	ns := now.UnixNano()
	type pen struct {
		label string
		until int64
	}
	pens := []pen{{"上游风控限制", a.restrictedTo.Load()}, {"熔断", a.breakerTo.Load()}, {"降权", a.degradeTo.Load()},
		{"余额保护", a.balanceTo.Load()}, {"软限流冷却", a.cooldownTo.Load()}}
	a.mu.Lock()
	soft, unscoped := a.g.softStreak, a.g.unscopedStreak
	if model != "" {
		pens = append(pens, pen{"模型 " + model + " 冷却", a.modelCool[model]})
	}
	a.mu.Unlock()
	best := pens[0]
	for _, p := range pens[1:] {
		if p.until > best.until {
			best = p
		}
	}
	if best.until > ns {
		streak := ""
		if best.label == "软限流冷却" && soft > 1 {
			streak = fmt.Sprintf("（连续第 %d 次）", soft)
		} else if strings.HasPrefix(best.label, "模型") && unscoped > 1 {
			streak = fmt.Sprintf("（连续第 %d 次）", unscoped)
		}
		return fmt.Sprintf("%s 剩余 %s%s", best.label, humanLeft(time.Duration(best.until-ns)), streak)
	}
	if a.guard.reserve.Load() {
		return "余额低于保留线"
	}
	if a.guard.dailyToken.Load() {
		return "今日 Token 额度用尽"
	}
	if model != "" && a.guard.creditBlocked(model) {
		return "今日积分额度用尽"
	}
	if model != "" && a.guard.modelTokenBlocked(model) {
		return "该模型今日额度用尽"
	}
	if remain, ok := a.CreditRemain(); ok && remain <= 0 {
		return "积分已用尽"
	}
	if exp := a.expiresAt.Load(); exp != 0 && exp-now.Unix() <= 120 {
		return "凭证已过期"
	}
	return ""
}

// NoteError records an error text without penalising the account.
func (a *Account) NoteError(msg, detail string) {
	a.mu.Lock()
	a.setErr(msg, detail)
	a.mu.Unlock()
	a.touch()
}
