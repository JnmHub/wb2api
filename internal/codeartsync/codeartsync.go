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
	for _, mi := range dis.Models {
		models = append(models, mi.ID)
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
		next := make(map[string]any, len(m)+4)
		for k, v := range m {
			next[k] = v
		}
		next["models"] = models
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

// Checkin claims the daily credits of one account and records the outcome in
// LastCheckin (the panel renders it there).
func (s *Service) Checkin(ctx context.Context, acc *pool.Account) (bool, string, error) {
	if acc == nil {
		return false, "", errors.New("account required")
	}
	rec := acc.Record()
	now := s.now()
	if s.stale(rec, now) {
		s.renew(ctx, acc)
		rec = acc.Record()
	}
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
	return !checkedInToday(rec.LastCheckin, s.now())
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
