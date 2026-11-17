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
	// Now is the clock (tests).
	Now func() time.Time
	Log *slog.Logger
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
	cred := codearts.CredentialOf(rec)
	benefitBase := s.BenefitBase
	if benefitBase == "" {
		benefitBase = codearts.BenefitBase
	}
	dis := codearts.DiscoverModelsWith(ctx, nil, s.base(rec), benefitBase, rec.UID, cred, s.ClaimBenefit)
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
	res, err := codearts.Checkin(ctx, nil, s.base(rec), codearts.CredentialOf(rec), now)

	message := res.Message
	if message == "" && err != nil {
		message = err.Error()
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
