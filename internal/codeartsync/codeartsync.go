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
	"sync"
	"time"

	"wb2api/internal/activity"
	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/refresh"
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
	// Refresh renews the current credential before expiry.
	Refresh func(context.Context, *pool.Account) error
	// RefreshObserved is used when the upstream rejects the observed AT.
	RefreshObserved func(context.Context, *pool.Account, *accounts.Record) error
	// Lead is how long before expiry the credential is renewed (0 = 10m).
	Lead time.Duration
	// Now is the clock (tests).
	Now func() time.Time
	Log *slog.Logger

	Activity          *activity.Log
	LoadCheckinRecord func(context.Context, string) (*accounts.Record, error)
	WithCheckinLock   func(context.Context, string, func(context.Context) error) error
	dailyLocks        [128]sync.Mutex
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

// credential may use a still-safe AT after a proactive refresh failure. Once
// the upstream rejects an AT, only a different usable generation can be sent.
func (s *Service) credential(ctx context.Context, acc *pool.Account, rejected *accounts.Record) (*accounts.Record, error) {
	rec := acc.Record()
	if rec.CodeArtsNeedsLogin() {
		return nil, refresh.ErrNeedsLogin
	}
	if rejected != nil {
		acc.RejectCodeArtsAccess(rejected)
	}
	if rec.CodeArtsAccessUsable(s.now()) && !acc.CodeArtsAccessRejected() &&
		(rejected != nil || !s.stale(rec, s.now())) {
		return rec, nil
	}
	var err error
	if rejected != nil && s.RefreshObserved != nil {
		err = s.RefreshObserved(ctx, acc, rejected)
	} else if s.Refresh != nil {
		err = s.Refresh(ctx, acc)
	}
	rec = acc.Record()
	if rec.CodeArtsNeedsLogin() || errors.Is(err, refresh.ErrNeedsLogin) {
		return nil, refresh.ErrNeedsLogin
	}
	if rec.CodeArtsAccessUsable(s.now()) && !acc.CodeArtsAccessRejected() &&
		(rejected == nil || rec.CodeArtsAccessVersion() != rejected.CodeArtsAccessVersion()) {
		return rec, nil
	}
	if err != nil {
		return nil, err
	}
	return nil, errors.New("CodeArts access credential unavailable")
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
	rec, err := s.credential(ctx, acc, nil)
	if err != nil {
		return codearts.Discovery{}, err
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
		replacement, err := s.credential(ctx, acc, rec)
		if err == nil {
			rec = replacement
			dis = discover()
			if expiredAnywhere(dis.Errors) {
				acc.RejectCodeArtsAccess(rec)
			}
		} else if len(dis.Models) == 0 {
			return dis, err
		}
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
	// models_meta keeps what discovery already knows (display name, context
	// window, output cap, description) so the panel can render a real table
	// instead of a column of dashes.
	meta := make([]any, 0, len(dis.Models))
	for _, mi := range dis.Models {
		models = append(models, mi.ID)
		if mi.Name != "" && mi.Name != mi.ID {
			names[mi.ID] = mi.Name
		}
		if mi.Benefit {
			benefit = append(benefit, mi.ID)
		}
		entry := map[string]any{"id": mi.ID}
		if mi.Name != "" {
			entry["name"] = mi.Name
		}
		if mi.ContextWindow > 0 {
			entry["context_window"] = mi.ContextWindow
		}
		if mi.MaxTokens > 0 {
			entry["max_tokens"] = mi.MaxTokens
		}
		if mi.Desc != "" {
			entry["description"] = mi.Desc
		}
		if mi.Benefit {
			entry["benefit"] = true
		}
		meta = append(meta, entry)
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
		next["models_meta"] = meta
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

// Balance reads the subscription statistics of one account and stores the
// aggregated credit position on the record, so the panel's credit column and
// the account drawer can show a real balance instead of a tick-in amount.
func (s *Service) Balance(ctx context.Context, acc *pool.Account) (codearts.Balance, error) {
	if acc == nil {
		return codearts.Balance{}, errors.New("account required")
	}
	rec, err := s.credential(ctx, acc, nil)
	if err != nil {
		return codearts.Balance{}, err
	}
	fetch := func() (*codearts.Statistics, error) {
		return codearts.FetchStatistics(ctx, nil, s.base(rec), codearts.CredentialOf(rec))
	}
	st, err := fetch()
	if err != nil && codearts.IsExpiredCredentialError(err) {
		replacement, renewErr := s.credential(ctx, acc, rec)
		if renewErr != nil {
			return codearts.Balance{}, renewErr
		}
		rec = replacement
		st, err = fetch()
		if codearts.IsExpiredCredentialError(err) {
			acc.RejectCodeArtsAccess(rec)
		}
	}
	if err != nil {
		return codearts.Balance{}, err
	}
	b := st.Aggregate()
	if serr := s.storeBalance(acc, b, s.now()); serr != nil {
		return b, serr
	}
	if b.HasRemaining && b.Remaining > 0 {
		acc.ReviveBalanceCooldown()
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
	next, ok := s.Pool.Update(acc.UID, func(r *accounts.Record) {
		if !b.HasRemaining {
			if previous, known := r.Credits["remain"]; known {
				credits["remain"] = previous
			}
		}
		r.Credits = credits
	})
	if !ok {
		return errors.New("account disappeared while storing the balance")
	}
	if s.Persist != nil {
		return s.Persist(next)
	}
	return nil
}
