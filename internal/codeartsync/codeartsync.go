// Package codeartsync wire the CodeArts channel's protocol client to the
// account pool: model discovery (three sources + benefit claim), the daily
// credit check-in, and the background scan that keeps the catalogue fresh.
//
// It exists as its own package so the admin API and the scheduler share one
// implementation: the same operation must not drift between the panel button
// and the unattended run.
package codeartsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream/codearts"
)

// Service performs the channel's account-level operations.
type Service struct {
	Pool    *pool.Pool
	Persist func(*accounts.Record) error
	// Base overrides the snap-access root (tests, mock).
	Base string
	// BenefitBase overrides the free-plan gateway root (tests, mock).
	BenefitBase string
	// ClaimBenefit claims the free-plan models before listing them. The claim
	// is idempotent upstream; without it those models answer 4004.200.
	ClaimBenefit bool
	// Refresh renews an account credential (nil = no renewer: calls fail the
	// way they always did). Discover and Checkin renew proactively when the STS
	// token is stale, and retry once when the upstream says it has expired.
	Refresh func(context.Context, *pool.Account) error
	// Lead is how long before expiry the credential is renewed (0 = 10m).
	Lead time.Duration
	// Now is the clock (tests).
	Now func() time.Time
	Log *slog.Logger
}

// defaultLead renews ten minutes before the STS token expires: a CodeArts
// credential lives two hours, and a stale one fails every call with APIG.0602.
const defaultLead = 10 * time.Minute

func (s *Service) lead() time.Duration {
	if s.Lead > 0 {
		return s.Lead
	}
	return defaultLead
}

// stale reports whether the credential needs renewing before use. An unknown
// expiry counts as stale: trying to refresh beats failing with 0602.
func (s *Service) stale(rec *accounts.Record, now time.Time) bool {
	if rec == nil {
		return true
	}
	if rec.ExpiresAt == 0 {
		return true
	}
	return !time.Unix(rec.ExpiresAt, 0).After(now.Add(s.lead()))
}

// renew runs the refresher, keeping the old credential on failure: a failed
// renewal must not turn a usable token into no token.
func (s *Service) renew(ctx context.Context, acc *pool.Account) {
	if s.Refresh == nil {
		return
	}
	if err := s.Refresh(ctx, acc); err != nil {
		s.logger().Warn("codearts credential renewal failed", "account", short(acc.UID), "err", err)
	}
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Service) base(rec *accounts.Record) string {
	if s.Base != "" {
		return s.Base
	}
	return realm.Get(rec.Realm).ChatUpstream
}

// Discover refreshes one account's catalogue and stores it on the record so
// /v1/models (and the benefit routing decision) sees the result. It returns the
// discovery even when some sources failed: a partial catalogue is still better
// than the previous one going stale.
func (s *Service) Discover(ctx context.Context, acc *pool.Account) (codearts.Discovery, error) {
	if acc == nil {
		return codearts.Discovery{}, errors.New("account required")
	}
	rec := acc.Record()
	now := s.now()
	if s.stale(rec, now) {
		s.renew(ctx, acc)
		rec = acc.Record()
	}
	benefitBase := s.BenefitBase
	if benefitBase == "" {
		benefitBase = codearts.BenefitBase
	}
	discover := func() codearts.Discovery {
		return codearts.DiscoverModelsWith(ctx, nil, s.base(rec), benefitBase, rec.UID, codearts.CredentialOf(rec), s.ClaimBenefit)
	}
	dis := discover()
	if expiredAnywhere(dis.Errors) {
		// The upstream rejected the STS token mid-flight: renew and retry once.
		s.renew(ctx, acc)
		rec = acc.Record()
		dis = discover()
	}
	if len(dis.Models) == 0 {
		if msg, ok := dis.Errors["all"]; ok {
			return dis, errors.New(msg)
		}
		return dis, errors.New("model discovery returned nothing")
	}
	if err := s.store(acc, dis); err != nil {
		return dis, err
	}
	return dis, nil
}

// store writes the merged catalogue back to the account record.
func (s *Service) store(acc *pool.Account, dis codearts.Discovery) error {
	models := make([]any, 0, len(dis.Models))
	benefit := make([]any, 0, len(dis.Models))
	names := make(map[string]any, len(dis.Models))
	for _, mi := range dis.Models {
		models = append(models, mi.ID)
		if mi.Name != "" && mi.Name != mi.ID {
			names[mi.ID] = mi.Name
		}
		if mi.Benefit {
			benefit = append(benefit, mi.ID)
		}
	}
	var problem any
	if len(dis.Errors) > 0 {
		raw, _ := json.Marshal(dis.Errors)
		problem = string(raw)
	}
	at := float64(s.now().Unix())
	next, ok := s.Pool.Update(acc.UID, func(r *accounts.Record) {
		m := codearts.Extra(r)
		if m == nil {
			m = map[string]any{}
		}
		// Copy before writing: the record and its Extra map are shared.
		next := make(map[string]any, len(m)+5)
		for k, v := range m {
			next[k] = v
		}
		next["models"] = models
		next["model_names"] = names
		next["benefit_models"] = benefit
		next["models_at"] = at
		if problem != nil {
			next["models_error"] = problem
		} else {
			delete(next, "models_error")
		}
		extra := make(map[string]any, len(r.Extra)+1)
		for k, v := range r.Extra {
			extra[k] = v
		}
		extra[codearts.ExtraKey] = next
		r.Extra = extra
	})
	if !ok {
		return errors.New("account disappeared during discovery")
	}
	if s.Persist != nil {
		return s.Persist(next)
	}
	return nil
}

// Claim write budget: the activity list is a write-capable endpoint, so an
// unattended loop must not hammer it. Manual requests bypass both limits.
const (
	maxClaimAttempts = 6
	minClaimGap      = 10 * time.Minute
	claimDayOffset   = 8 * 3600 // the activity period is UTC+8 midnight
)

// claimLedger is the per-account, per-day write budget.
type claimLedger struct {
	Day      string `json:"day"`
	Attempts int    `json:"attempts"`
	LastAt   int64  `json:"last_at"`
}

// claimDay reports the activity day of t (UTC+8).
func claimDay(t time.Time) string {
	return t.In(time.FixedZone("CST", claimDayOffset)).Format("2006-01-02")
}

func ledgerOf(rec *accounts.Record, now time.Time) claimLedger {
	led := claimLedger{}
	if m := codearts.Extra(rec); m != nil {
		if raw, ok := m["claim_ledger"].(map[string]any); ok {
			led.Day, _ = raw["day"].(string)
			if v, ok := raw["attempts"].(float64); ok {
				led.Attempts = int(v)
			}
			if v, ok := raw["last_at"].(float64); ok {
				led.LastAt = int64(v)
			}
		}
	}
	if led.Day != claimDay(now) {
		led = claimLedger{Day: claimDay(now)}
	}
	return led
}

// gapRemaining is how long until the next automatic attempt may run.
func (l claimLedger) gapRemaining(now time.Time) time.Duration {
	if l.LastAt == 0 {
		return 0
	}
	next := time.Unix(l.LastAt, 0).Add(minClaimGap)
	if now.Before(next) {
		return next.Sub(now)
	}
	return 0
}

// recordAttempt bumps the write budget and stores it on the record.
func (s *Service) recordAttempt(acc *pool.Account, now time.Time) {
	led := ledgerOf(acc.Record(), now)
	led.Attempts++
	led.LastAt = now.Unix()
	store := map[string]any{"day": led.Day, "attempts": led.Attempts, "last_at": float64(led.LastAt)}
	next, ok := s.Pool.Update(acc.UID, func(r *accounts.Record) {
		m := codearts.Extra(r)
		cp := make(map[string]any, len(m)+1)
		for k, v := range m {
			cp[k] = v
		}
		cp["claim_ledger"] = store
		extra := make(map[string]any, len(r.Extra)+1)
		for k, v := range r.Extra {
			extra[k] = v
		}
		extra[codearts.ExtraKey] = cp
		r.Extra = extra
	})
	if ok && s.Persist != nil {
		_ = s.Persist(next)
	}
}

// Checkin claims the daily credits of one account and records the outcome in
// LastCheckin (the panel renders it there). manual bypasses the daily write
// budget, which only exists to keep the unattended loop polite.
func (s *Service) Checkin(ctx context.Context, acc *pool.Account, manual bool) (bool, string, error) {
	if acc == nil {
		return false, "", errors.New("account required")
	}
	rec := acc.Record()
	now := s.now()
	if !manual {
		led := ledgerOf(rec, now)
		if led.Attempts >= maxClaimAttempts {
			return true, "今日领取尝试已达上限（6 次），明天再试", nil
		}
		if gap := led.gapRemaining(now); gap > 0 {
			return true, fmt.Sprintf("距上次尝试不足 10 分钟，%s 后可再试", gap.Round(time.Second)), nil
		}
	}
	if s.stale(rec, now) {
		s.renew(ctx, acc)
		rec = acc.Record()
	}
	s.recordAttempt(acc, now)
	res, err := codearts.Checkin(ctx, nil, s.base(rec), codearts.CredentialOf(rec), now)
	if err != nil && codearts.IsExpiredCredentialError(err) {
		// One renewal, one retry: this is the "idle for two hours" case.
		s.renew(ctx, acc)
		rec = acc.Record()
		res, err = codearts.Checkin(ctx, nil, s.base(rec), codearts.CredentialOf(rec), now)
	}

	// A second pass the same day (the scheduler racing a manual click) finds
	// nothing claimable and would overwrite a real "+4000" with "无可领取". Keep
	// the earlier success visible instead of downgrading it.
	if res.Credits == 0 && len(res.Campaigns) == 0 && err == nil {
		if ok, at, credits, msg := lastCheckinParts(rec.LastCheckin); ok && sameLocalDay(at, now) {
			res.Message, res.Credits = msg, credits
		}
	}

	message := res.Message
	if message == "" && err != nil {
		message = err.Error()
	}
	if err != nil && codearts.IsExpiredCredentialError(err) {
		message = "凭据已过期，自动续期未成功（需要重新登录该账号）"
	}
	status := map[string]any{
		"at":      float64(now.Unix()),
		"channel": realm.CodeArts,
		"ok":      res.OK && err == nil,
		"message": message,
	}
	if res.Credits > 0 {
		status["credits"] = res.Credits
	}
	if next, ok := s.Pool.Update(acc.UID, func(r *accounts.Record) { r.LastCheckin = status }); ok && s.Persist != nil {
		if serr := s.Persist(next); serr != nil && err == nil {
			err = serr
		}
	}
	if err != nil {
		return false, message, err
	}
	if !res.OK {
		return false, message, errors.New(message)
	}
	return true, message, nil
}

// CanCheckin reports whether the account still needs today's check-in. The
// upstream decides eligibility; this only avoids repeating a finished day.
func (s *Service) CanCheckin(acc *pool.Account) bool {
	if acc == nil {
		return false
	}
	rec := acc.Record()
	if !realm.IsCodeArts(rec.Realm) || !rec.IsEnabled() {
		return false
	}
	now := s.now()
	led := ledgerOf(rec, now)
	if led.Attempts >= maxClaimAttempts || led.gapRemaining(now) > 0 {
		return false
	}
	return !checkedInToday(rec.LastCheckin, now)
}

func checkedInToday(v any, now time.Time) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	okFlag, _ := m["ok"].(bool)
	if !okFlag {
		return false
	}
	at, _ := m["at"].(float64)
	if at == 0 {
		return false
	}
	ts := time.Unix(int64(at), 0)
	y, mo, d := ts.Date()
	y2, mo2, d2 := now.Date()
	return y == y2 && mo == mo2 && d == d2
}

// lastCheckinParts reads the stamp written by Checkin.
func lastCheckinParts(v any) (ok bool, at time.Time, credits float64, message string) {
	m, isMap := v.(map[string]any)
	if !isMap {
		return false, time.Time{}, 0, ""
	}
	okFlag, _ := m["ok"].(bool)
	sec, _ := m["at"].(float64)
	credits, _ = m["credits"].(float64)
	message, _ = m["message"].(string)
	if sec == 0 {
		return false, time.Time{}, 0, ""
	}
	return okFlag, time.Unix(int64(sec), 0), credits, message
}

// expiredAnywhere reports whether any discovery source failed because the STS
// token expired.
func expiredAnywhere(errs map[string]string) bool {
	for _, msg := range errs {
		if codearts.IsExpiredCredentialError(errors.New(msg)) {
			return true
		}
	}
	return false
}

func sameLocalDay(a, b time.Time) bool {
	if a.IsZero() {
		return false
	}
	ay, am, ad := a.Local().Date()
	by, bm, bd := b.Local().Date()
	return ay == by && am == bm && ad == bd
}

// Balance reads the subscription statistics of one account and stores the
// aggregated credit position on the record, so the panel's credit column and
// the account drawer can show a real balance instead of a tick-in amount.
func (s *Service) Balance(ctx context.Context, acc *pool.Account) (codearts.Balance, error) {
	if acc == nil {
		return codearts.Balance{}, errors.New("account required")
	}
	rec := acc.Record()
	now := s.now()
	if s.stale(rec, now) {
		s.renew(ctx, acc)
		rec = acc.Record()
	}
	fetch := func() (*codearts.Statistics, error) {
		return codearts.FetchStatistics(ctx, nil, s.base(rec), codearts.CredentialOf(rec))
	}
	st, err := fetch()
	if err != nil && codearts.IsExpiredCredentialError(err) {
		s.renew(ctx, acc)
		rec = acc.Record()
		st, err = fetch()
	}
	if err != nil {
		return codearts.Balance{}, err
	}
	b := st.Aggregate()
	if serr := s.storeBalance(acc, b, now); serr != nil {
		return b, serr
	}
	return b, nil
}

// storeBalance writes the aggregated position into the shared record shape the
// panel already renders (remain / total / used / plan / reset).
func (s *Service) storeBalance(acc *pool.Account, b codearts.Balance, now time.Time) error {
	credits := map[string]any{
		"source":     "codearts",
		"updated_at": float64(now.Unix()),
	}
	if b.HasRemaining {
		credits["remain"] = b.Remaining
	}
	if b.HasTotal {
		credits["total"] = b.Total
	}
	if b.HasRemaining && b.HasTotal {
		credits["used"] = b.Used
	}
	if b.Plan != "" {
		credits["plan_code"] = b.Plan
	}
	if b.PlanName != "" {
		credits["plan"] = b.PlanName
	}
	if b.ResetDate != "" {
		credits["reset"] = b.ResetDate
	}
	meters := make([]any, 0, len(b.Meters))
	for _, m := range b.Meters {
		row := map[string]any{"name": m.Name, "label": m.Label, "view": m.BalanceView()}
		if m.CreditRemain != nil {
			row["remain"] = *m.CreditRemain
		}
		if m.CreditTotal != nil {
			row["total"] = *m.CreditTotal
		}
		if m.UsedPercent != nil {
			row["used_percent"] = *m.UsedPercent
		}
		meters = append(meters, row)
	}
	if len(meters) > 0 {
		credits["meters"] = meters
	}
	next, ok := s.Pool.Update(acc.UID, func(r *accounts.Record) { r.Credits = credits })
	if !ok {
		return errors.New("account disappeared while storing the balance")
	}
	if s.Persist != nil {
		return s.Persist(next)
	}
	return nil
}
