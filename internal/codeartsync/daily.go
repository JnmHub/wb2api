package codeartsync

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"wb2api/internal/activity"
	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/refresh"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream/codearts"
)

const (
	DailyClaimed      = "claimed"
	DailyAlready      = "already_claimed"
	DailyUnavailable  = "unavailable"
	DailyPending      = "pending_confirmation"
	DailyWaiting      = "waiting_retry"
	DailyNeedsLogin   = "needs_login"
	DailyExhausted    = "attempts_exhausted"
	DailySkipped      = "skipped"
	maxClaimAttempts  = 6
	minClaimGap       = 10 * time.Minute
	availabilityDelay = time.Hour
	balanceRetryGap   = 30 * time.Minute
)

var beijing = time.FixedZone("Asia/Shanghai", 8*60*60)

type DailyResult struct {
	At        int64   `json:"at"`
	Day       string  `json:"day"`
	Channel   string  `json:"channel"`
	State     string  `json:"state"`
	OK        bool    `json:"ok"`
	Message   string  `json:"message"`
	Credits   float64 `json:"credits,omitempty"`
	NextRetry int64   `json:"next_retry_at,omitempty"`
}

type campaignProgress struct {
	Writes        int     `json:"writes"`
	ClaimStarted  bool    `json:"claim_started,omitempty"`
	ClaimAccepted bool    `json:"claim_accepted,omitempty"`
	Expected      float64 `json:"expected,omitempty"`
	Confirmed     bool    `json:"confirmed,omitempty"`
}

type dailyLedger struct {
	DailyResult
	Progress       map[string]campaignProgress `json:"daily_progress,omitempty"`
	BalanceRetryAt int64                       `json:"balance_retry_at,omitempty"`
}

type dailyCredentialsError struct{ cause error }

func (e dailyCredentialsError) Error() string { return e.cause.Error() }
func (e dailyCredentialsError) Unwrap() error { return e.cause }

func claimDay(now time.Time) string { return now.In(beijing).Format("2006-01-02") }

func dailyOf(rec *accounts.Record, now time.Time) dailyLedger {
	day := claimDay(now)
	var ledger dailyLedger
	if raw, err := json.Marshal(rec.LastCheckin); err == nil {
		_ = json.Unmarshal(raw, &ledger)
	}
	if ledger.Day != day || ledger.Channel != realm.CodeArts {
		ledger = dailyLedger{DailyResult: DailyResult{Day: day, Channel: realm.CodeArts}}
	}
	if ledger.Progress == nil {
		ledger.Progress = make(map[string]campaignProgress)
	}
	return ledger
}

func (l dailyLedger) due(rec *accounts.Record, now time.Time) bool {
	if l.State == DailyClaimed || l.State == DailyAlready {
		return false
	}
	if rec.CodeArtsNeedsLogin() {
		return l.State != DailyNeedsLogin
	}
	switch l.State {
	case DailyNeedsLogin:
		return true // a replacement credential can resume the same day
	default:
		return l.NextRetry <= now.Unix()
	}
}

func (l dailyLedger) balanceDue(rec *accounts.Record, now time.Time) bool {
	if !l.OK || rec.CodeArtsNeedsLogin() || l.BalanceRetryAt > now.Unix() {
		return false
	}
	remaining, err := strconv.ParseFloat(fmt.Sprint(rec.Credits["remain"]), 64)
	return err == nil && remaining <= 0
}

func (s *Service) CanCheckin(acc *pool.Account) bool {
	if acc == nil {
		return false
	}
	rec := acc.Record()
	now := s.now()
	ledger := dailyOf(rec, now)
	return realm.IsCodeArts(rec.Realm) && rec.IsEnabled() && (ledger.due(rec, now) || ledger.balanceDue(rec, now))
}

func DailyStatus(rec *accounts.Record, now time.Time) DailyResult {
	return dailyOf(rec, now).DailyResult
}

// Checkin preserves the account action's existing response contract.
func (s *Service) Checkin(ctx context.Context, acc *pool.Account, manual bool) (bool, string, error) {
	trigger := "scheduler"
	if manual {
		trigger = "manual"
	}
	res, err := s.RunDaily(ctx, acc, trigger)
	if err == nil && !res.OK {
		err = errors.New(res.Message)
	}
	return res.OK, res.Message, err
}

// RunDaily serializes a day's upstream writes across manual, scheduled, and
// shared-store runners. A second runner always starts from the durable ledger.
func (s *Service) RunDaily(ctx context.Context, acc *pool.Account, trigger string) (DailyResult, error) {
	if acc == nil {
		return DailyResult{State: DailySkipped, Message: "账号不存在"}, errors.New("account required")
	}
	lock := &s.dailyLocks[int(sha256.Sum256([]byte(acc.UID))[0])%len(s.dailyLocks)]
	if !lock.TryLock() {
		return DailyResult{State: DailySkipped, Message: "签到正在执行，稍后可查看结果"}, nil
	}
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var out DailyResult
	run := func(ctx context.Context) error {
		rec, err := s.loadCheckin(ctx, acc)
		if err != nil {
			return err
		}
		if !realm.IsCodeArts(rec.Realm) || !rec.IsEnabled() || (trigger != "manual" && !rec.TasksOn()) {
			out = DailyResult{State: DailySkipped, Message: "账号未启用 CodeArts 签到"}
			return nil
		}
		now := s.now()
		if acc.RestrictedUntil().After(now) {
			out = DailyResult{State: DailySkipped, Message: "账号暂处于上游限制中"}
			return nil
		}
		ledger := dailyOf(rec, now)
		if !ledger.due(rec, now) {
			out = ledger.DailyResult
			if ledger.balanceDue(rec, now) {
				ledger.BalanceRetryAt = now.Add(balanceRetryGap).Unix()
				if err := s.saveDaily(ctx, acc, ledger); err != nil {
					s.logger().Warn("CodeArts balance retry could not be scheduled", "account", short(acc.UID), "error", activity.SafeMessage(err.Error()))
					return nil
				}
				s.refreshDailyBalance(ctx, acc)
			}
			return nil
		}
		if err := s.runDaily(ctx, acc, rec, &ledger, trigger, &out); err != nil {
			return err
		}
		if out.OK {
			s.refreshDailyBalance(ctx, acc)
		}
		return nil
	}
	var err error
	if s.WithCheckinLock != nil {
		err = s.WithCheckinLock(ctx, acc.UID, run)
	} else {
		err = run(ctx)
	}
	if err != nil && out.Message == "" {
		out = DailyResult{State: DailyWaiting, Message: activity.SafeMessage(err.Error())}
	}
	return out, err
}

func (s *Service) refreshDailyBalance(ctx context.Context, acc *pool.Account) {
	balanceCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := s.Balance(balanceCtx, acc); err != nil {
		s.logger().Warn("CodeArts balance refresh after sign-in failed", "account", short(acc.UID), "error", activity.SafeMessage(err.Error()))
	}
}

func (s *Service) loadCheckin(ctx context.Context, acc *pool.Account) (*accounts.Record, error) {
	if s.LoadCheckinRecord != nil {
		return s.LoadCheckinRecord(ctx, acc.UID)
	}
	if s.Pool == nil || s.Pool.Get(acc.UID) != acc {
		return nil, errors.New("签到账号已变更")
	}
	return acc.Record(), nil
}

func (s *Service) saveDaily(ctx context.Context, acc *pool.Account, ledger dailyLedger) error {
	rec, err := s.loadCheckin(ctx, acc)
	if err != nil {
		return err
	}
	if !realm.IsCodeArts(rec.Realm) {
		return errors.New("签到账号所属渠道已变更")
	}
	raw, err := json.Marshal(ledger)
	if err != nil {
		return err
	}
	var stamp map[string]any
	if err := json.Unmarshal(raw, &stamp); err != nil {
		return err
	}
	next := *rec
	next.LastCheckin = stamp
	if s.Persist != nil {
		if err := s.Persist(&next); err != nil {
			return fmt.Errorf("保存签到状态失败: %w", err)
		}
	}
	if s.LoadCheckinRecord == nil {
		if _, ok := s.Pool.Update(acc.UID, func(r *accounts.Record) {
			r.LastCheckin = stamp
			if r.Path == "" {
				r.Path = next.Path
			}
		}); !ok {
			return errors.New("签到账号已移除")
		}
	}
	return nil
}

func (s *Service) runDaily(ctx context.Context, acc *pool.Account, rec *accounts.Record, ledger *dailyLedger, trigger string, out *DailyResult) error {
	now := s.now()
	ledger.At = now.Unix()
	ledger.OK = false
	ledger.NextRetry = 0

	finish := func(state, msg string, retry time.Time) error {
		ledger.State, ledger.Message = state, activity.SafeMessage(msg)
		ledger.OK = state == DailyClaimed || state == DailyAlready
		if ledger.OK {
			ledger.BalanceRetryAt = s.now().Add(balanceRetryGap).Unix()
		}
		ledger.NextRetry = 0
		if !retry.IsZero() {
			ledger.NextRetry = retry.Unix()
		}
		if err := s.saveDaily(ctx, acc, *ledger); err != nil {
			return err
		}
		*out = ledger.DailyResult
		s.Activity.Record(activity.Row{
			UID: rec.UID, Nickname: rec.Nickname, Realm: rec.Realm, Task: activity.TaskCodeArts,
			Trigger: trigger, OK: ledger.OK, Message: ledger.Message, State: state,
			Day: ledger.Day, Credits: ledger.Credits, NextRetry: ledger.NextRetry,
		})
		return nil
	}
	wait := func(state, msg string, cause error, delay time.Duration) error {
		next := s.now().Add(delay)
		if later := codearts.CreditRetryTime(cause, s.now()); later.After(next) {
			next = later
		}
		if later, ok := refresh.RetryTime(cause); ok && later.After(next) {
			next = later
		}
		return finish(state, msg, next)
	}

	renewed := false
	renew := func(rejected bool) error {
		if rec.CodeArtsNeedsLogin() {
			return refresh.ErrNeedsLogin
		}
		if renewed {
			return errors.New("暂时无法取得可用的 CodeArts 访问凭据")
		}
		renewed = true
		var err error
		switch {
		case rejected && s.RefreshObserved != nil:
			err = s.RefreshObserved(ctx, acc, rec)
		case s.Refresh != nil:
			err = s.Refresh(ctx, acc)
		default:
			err = errors.New("CodeArts 凭据刷新服务未启用")
		}
		if err != nil {
			return err
		}
		fresh, err := s.loadCheckin(ctx, acc)
		if err != nil {
			return err
		}
		rec = fresh
		if rec.CodeArtsNeedsLogin() {
			return refresh.ErrNeedsLogin
		}
		if !rec.CodeArtsAccessUsable(s.now()) {
			return errors.New("刷新后未取得可用的 CodeArts 访问凭据")
		}
		return nil
	}
	authFailure := func(err error) error {
		if errors.Is(err, refresh.ErrNeedsLogin) || rec.CodeArtsNeedsLogin() {
			return finish(DailyNeedsLogin, "刷新凭据已失效，需要重新登录", time.Time{})
		}
		return wait(DailyWaiting, "等待 CodeArts 凭据恢复", err, minClaimGap)
	}
	local := acc.Record()
	rejected := local.CodeArtsAccessVersion() == rec.CodeArtsAccessVersion() && acc.CodeArtsAccessRejected()
	if rejected || !rec.CodeArtsAccessUsable(now) {
		if err := renew(false); err != nil {
			return authFailure(err)
		}
	}

	hc := &http.Client{Timeout: 20 * time.Second}
	crossedDay := errors.New("北京时间已跨日，等待新一轮签到")
	call := func(fn func(string, codearts.Credential) error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if claimDay(s.now()) != ledger.Day {
			return crossedDay
		}
		err := fn(s.base(rec), codearts.CredentialOf(rec))
		if !codearts.IsExpiredCredentialError(err) {
			return err
		}
		if err := renew(true); err != nil {
			return dailyCredentialsError{err}
		}
		if claimDay(s.now()) != ledger.Day {
			return crossedDay
		}
		return fn(s.base(rec), codearts.CredentialOf(rec))
	}
	var campaigns []codearts.Campaign
	fetch := func() error {
		return call(func(base string, cred codearts.Credential) error {
			var err error
			campaigns, err = codearts.FetchCampaigns(ctx, hc, base, cred)
			return err
		})
	}
	recordConfirmed := func(c codearts.Campaign) {
		progress := ledger.Progress[c.Key()]
		if !progress.Confirmed && progress.ClaimAccepted {
			ledger.Credits += progress.Expected
		}
		progress.Confirmed = true
		ledger.Progress[c.Key()] = progress
	}
	fail := func(stage string, err error) error {
		if errors.Is(err, crossedDay) || ctx.Err() != nil {
			return err
		}
		if errors.Is(err, refresh.ErrNeedsLogin) || rec.CodeArtsNeedsLogin() {
			return authFailure(err)
		}
		var credentialErr dailyCredentialsError
		if errors.As(err, &credentialErr) || codearts.IsExpiredCredentialError(err) {
			return authFailure(err)
		}
		if stage == "discovery" {
			return wait(DailyWaiting, "读取官方签到活动失败，等待重试", err, minClaimGap)
		}
		var businessErr *codearts.CreditBusinessError
		if errors.As(err, &businessErr) {
			return wait(DailyWaiting, fmt.Sprintf("官方签到操作未接受（业务码 %d），等待重试", businessErr.Code), err, minClaimGap)
		}
		return wait(DailyPending, "官方活动尚未确认到账，等待核对", err, minClaimGap)
	}
	if err := fetch(); err != nil {
		return fail("discovery", err)
	}
	if claimDay(s.now()) != ledger.Day {
		return crossedDay
	}

	daily := make(map[string]codearts.Campaign)
	for _, campaign := range campaigns {
		if id := campaign.Key(); id != "" && campaign.IsDailyCredit() && campaign.WithinPeriod(now) {
			daily[id] = campaign
		}
	}
	if len(daily) == 0 {
		if len(ledger.Progress) > 0 {
			return wait(DailyPending, "官方活动暂未返回，等待核对", nil, minClaimGap)
		}
		return wait(DailyUnavailable, "今天暂无可识别的每日积分活动", nil, availabilityDelay)
	}

	ids := make([]string, 0, len(daily))
	for id := range daily {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	prepare := func(id string, progress campaignProgress) error {
		progress.Writes++
		ledger.Progress[id] = progress
		ledger.State, ledger.Message = DailyPending, "官方活动尚未确认到账，等待核对"
		ledger.NextRetry = s.now().Add(minClaimGap).Unix()
		return s.saveDaily(ctx, acc, *ledger)
	}
	wrote, exhausted, unavailable := false, false, false
	for _, id := range ids {
		campaign := daily[id]
		progress := ledger.Progress[id]
		if campaign.Confirmed() {
			recordConfirmed(campaign)
			continue
		}
		if progress.Confirmed {
			continue
		}
		if campaign.State() != "CLAIMED" && !progress.ClaimAccepted && campaign.Claimable {
			// Keep a write available for confirmation after an accepted claim.
			if progress.Writes >= maxClaimAttempts-1 {
				exhausted = true
				continue
			}
			if !progress.ClaimStarted {
				progress.ClaimStarted = true
				progress.Expected = campaign.PendingTotalAmount
				if progress.Expected <= 0 {
					progress.Expected = campaign.BenefitAmount
				}
			}
			if err := prepare(id, progress); err != nil {
				return err
			}
			key := sha256.Sum256([]byte(rec.UID + "\x00" + ledger.Day + "\x00" + id))
			if err := call(func(base string, cred codearts.Credential) error {
				return codearts.ClaimCredit(ctx, hc, base, cred, campaign, fmt.Sprintf("claim_%x", key[:16]))
			}); err != nil {
				return fail("claim", err)
			}
			progress = ledger.Progress[id]
			progress.ClaimAccepted = true
		} else if campaign.State() != "CLAIMED" && !progress.ClaimAccepted {
			unavailable = true
			continue
		}
		if progress.Writes >= maxClaimAttempts {
			exhausted = true
			continue
		}
		if err := prepare(id, progress); err != nil {
			return err
		}
		if err := call(func(base string, cred codearts.Credential) error {
			return codearts.ConfirmCredit(ctx, hc, base, cred, campaign)
		}); err != nil {
			return fail("confirm", err)
		}
		wrote = true
	}
	if wrote {
		if err := fetch(); err != nil {
			return fail("verification", err)
		}
		if claimDay(s.now()) != ledger.Day {
			return crossedDay
		}
		for _, campaign := range campaigns {
			if _, tracked := daily[campaign.Key()]; tracked && campaign.IsDailyCredit() && campaign.Confirmed() {
				recordConfirmed(campaign)
			}
		}
	}
	confirmed := 0
	for _, id := range ids {
		if ledger.Progress[id].Confirmed {
			confirmed++
		}
	}
	if confirmed == len(ids) {
		if ledger.Credits > 0 {
			return finish(DailyClaimed, fmt.Sprintf("官方已确认到账 %g 积分", ledger.Credits), time.Time{})
		}
		return finish(DailyAlready, "官方活动已确认领取", time.Time{})
	}
	if exhausted {
		// Continue read-only verification in case the last write took effect or
		// a delayed claim now allows confirmation with its reserved write.
		return wait(DailyExhausted, "今日写入次数已达上限，等待官方确认", nil, availabilityDelay)
	}
	if wrote || len(ledger.Progress) > confirmed {
		return wait(DailyPending, "官方活动尚未确认到账，等待核对", nil, minClaimGap)
	}
	if unavailable {
		return wait(DailyUnavailable, "官方每日积分活动暂不可领取", nil, availabilityDelay)
	}
	return wait(DailyWaiting, "签到状态待更新", nil, minClaimGap)
}
