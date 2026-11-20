// Package raccoonsync wires the raccoon channel's protocol client to the
// account pool: model discovery, the daily points link, the two one-off sign-up
// grants, credit balances and the background scan that keeps the catalogue
// fresh.
//
// It exists as its own package for the same reason as codeartsync: the admin
// API and the scheduler must share one implementation, or the panel button and
// the unattended run drift apart.
package raccoonsync

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/refresh"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream/raccoon"
)

// Discovery is the outcome of one catalogue refresh.
type Discovery struct {
	Models []string
	At     time.Time
	Err    error
	// FromFallback marks a result that kept the previous list.
	Kept bool
}

// ClaimResult is the outcome of one sign-up claim run.
type ClaimResult struct {
	Outcomes []raccoon.ClaimOutcome
	Earned   int64
	Settled  map[string]int64
}

// Service performs the channel's account-level operations.
type Service struct {
	Pool    *pool.Pool
	Persist func(*accounts.Record) error
	// Refresh renews the credential before use; RefreshObserved is used when
	// the upstream rejected the observed token.
	Refresh         func(context.Context, *pool.Account) error
	RefreshObserved func(context.Context, *pool.Account, *accounts.Record) error
	// Client overrides the HTTP client (tests, staging).
	Client *raccoon.Client
	// Now is the clock (tests).
	Now func() time.Time
	Log *slog.Logger
	// WithAccountLock serialises the daily task per account (nil = local mutex).
	WithAccountLock func(context.Context, string, func(context.Context) error) error
	// LoadLatest reads the committed task ledger after taking a shared lock.
	LoadLatest func(context.Context, string) (*accounts.Record, error)

	daily [64]sync.Mutex
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

func (s *Service) client() *raccoon.Client {
	if s.Client != nil {
		return s.Client
	}
	return raccoon.New(&http.Client{Timeout: raccoon.DefaultTimeout})
}

// lock serialises one account's daily work; without a shared lock the local
// striping mutex keeps a single replica from running two grants at once.
func (s *Service) lock(ctx context.Context, acc *pool.Account, fn func(context.Context) error) error {
	uid := acc.UID
	if s.WithAccountLock != nil {
		return s.WithAccountLock(ctx, uid, func(ctx context.Context) error {
			if s.LoadLatest != nil {
				current := acc.Record()
				latest, err := s.LoadLatest(ctx, uid)
				if err != nil {
					return err
				}
				acc.ReplaceRaccoonRecord(s.Pool, current, latest)
			}
			return fn(ctx)
		})
	}
	h := fnv(uid) % uint32(len(s.daily))
	m := &s.daily[h]
	m.Lock()
	defer m.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(ctx)
}

func fnv(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// CanCheckin reports whether the account may take part in the daily task.
func (s *Service) CanCheckin(acc *pool.Account) bool {
	if acc == nil {
		return false
	}
	rec := acc.Record()
	if rec == nil || !realm.IsRaccoon(rec.Realm) || !rec.IsEnabled() || !rec.TasksOn() {
		return false
	}
	now := s.now()
	cred := raccoon.CredentialOf(rec)
	return cred.Token != "" && !rec.RaccoonNeedsLogin() &&
		(raccoon.AccessUsable(rec, now) || (cred.RefreshToken != "" && !now.Before(rec.RaccoonAuthState().RetryAt)))
}

// credential returns a usable record for one operation, renewing first when the
// stored token is stale or was rejected.
func (s *Service) credential(ctx context.Context, acc *pool.Account, rejected *accounts.Record) (*accounts.Record, error) {
	rec := acc.Record()
	if s.Pool != nil && s.Pool.Get(acc.UID) != acc {
		return nil, accounts.ErrDeleted
	}
	if rec == nil || !realm.IsRaccoon(rec.Realm) || rec.RaccoonNeedsLogin() {
		return nil, refresh.ErrNeedsLogin
	}
	manual := refresh.IsManualRaccoon(ctx, acc.UID)
	if !acc.Enabled() && !manual {
		return nil, refresh.ErrRaccoonDisabled
	}
	if rejected == nil && raccoon.AccessUsable(rec, s.now()) {
		return rec, nil
	}
	var err error
	switch {
	case rejected != nil && s.RefreshObserved != nil:
		err = s.RefreshObserved(ctx, acc, rejected)
	case s.Refresh != nil:
		err = s.Refresh(ctx, acc)
	default:
		err = refresh.ErrNeedsLogin
	}
	if err != nil {
		return nil, err
	}
	if !acc.Enabled() && !manual {
		return nil, refresh.ErrRaccoonDisabled
	}
	rec = acc.Record()
	if !raccoon.AccessUsable(rec, s.now()) {
		return nil, refresh.ErrNeedsLogin
	}
	return rec, nil
}

// useCredential retries one operation on the replacement credential after a
// genuine upstream rejection of the old one.
func useCredential[T any](ctx context.Context, s *Service, acc *pool.Account,
	run func(*accounts.Record) (T, error)) (T, *accounts.Record, error) {
	var out T
	rec, err := s.credential(ctx, acc, nil)
	if err != nil {
		return out, nil, err
	}
	out, err = run(rec)
	if !errors.Is(err, raccoon.ErrNeedsLogin) {
		return out, rec, err
	}
	observed := rec
	rec, err = s.credential(ctx, acc, observed)
	if err != nil {
		return out, observed, err
	}
	out, err = run(rec)
	return out, rec, err
}

// Discover refreshes the account's model catalogue. A failure keeps the
// previous list and records why, so the panel can say "stale" instead of
// showing an empty picker.
func (s *Service) Discover(ctx context.Context, acc *pool.Account) Discovery {
	models, rec, fetchErr := useCredential(ctx, s, acc, func(rec *accounts.Record) ([]raccoon.Model, error) {
		return s.client().FetchCatalog(ctx, raccoon.CredentialOf(rec))
	})
	now := s.now()
	if fetchErr != nil {
		if rec != nil && !errors.Is(fetchErr, raccoon.ErrNeedsLogin) && !errors.Is(fetchErr, refresh.ErrNeedsLogin) {
			err := s.save(acc, rec, func(latest *accounts.Record) *accounts.Record {
				return withModels(latest, latest.RaccoonFields()["models"], now, fetchErr.Error())
			})
			fetchErr = errors.Join(fetchErr, err)
		}
		return Discovery{Err: fetchErr, Kept: true, At: now}
	}
	ids := raccoon.ModelsOf(models)
	visible := VisibleModels(models)
	if err := s.save(acc, rec, func(latest *accounts.Record) *accounts.Record {
		return withModelsAndVisibility(latest, ids, visible, raccoon.EncodeMeta(models), now, "")
	}); err != nil {
		return Discovery{Err: err, Kept: true}
	}
	return Discovery{Models: ids, At: now}
}

// VisibleModels reports the advertised subset of a discovery result.
func VisibleModels(models []raccoon.Model) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		if m.Kind == "chat" && m.Visible && m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out
}

// Checkin runs the daily points link and stores the outcome plus the balance.
func (s *Service) Checkin(ctx context.Context, acc *pool.Account) (raccoon.CheckinResult, error) {
	var out raccoon.CheckinResult
	err := s.lock(ctx, acc, func(ctx context.Context) error {
		if !refresh.IsManualRaccoon(ctx, acc.UID) && checkinRecorded(acc.Record(), s.now()) {
			out = raccoon.CheckinResult{Success: true, AlreadyCompleted: true, Message: "今日签到已确认入账"}
			return nil
		}
		res, rec, err := useCredential(ctx, s, acc, func(rec *accounts.Record) (raccoon.CheckinResult, error) {
			return s.client().ClaimDaily(ctx, raccoon.CredentialOf(rec), s.now())
		})
		if err != nil {
			return err
		}
		out = res
		if err := s.save(acc, rec, func(latest *accounts.Record) *accounts.Record {
			return withCheckin(latest, res, s.now())
		}); err != nil {
			return err
		}
		// A balance error is independent of the verified daily grant.
		_, _ = s.Credits(ctx, acc)
		return nil
	})
	return out, err
}

// Claim probes the sign-up grants that are not settled yet.
func (s *Service) Claim(ctx context.Context, acc *pool.Account) (ClaimResult, error) {
	var out ClaimResult
	err := s.lock(ctx, acc, func(ctx context.Context) error {
		var outcomes []raccoon.ClaimOutcome
		_, rec, err := useCredential(ctx, s, acc, func(rec *accounts.Record) ([]raccoon.ClaimOutcome, error) {
			settled := raccoon.SettleOutcomes(settledOf(rec), outcomes, s.now())
			partial, err := s.client().ClaimSignup(ctx, raccoon.CredentialOf(rec), settled)
			outcomes = append(outcomes, partial...)
			return partial, err
		})
		out.Outcomes = outcomes
		for _, o := range out.Outcomes {
			if o.Granted {
				out.Earned += o.Points
			}
		}
		if rec != nil && len(outcomes) > 0 {
			saveErr := s.save(acc, rec, func(latest *accounts.Record) *accounts.Record {
				out.Settled = raccoon.SettleOutcomes(settledOf(latest), outcomes, s.now())
				return withClaims(latest, out.Settled, s.now())
			})
			return errors.Join(err, saveErr)
		}
		if rec != nil {
			out.Settled = settledOf(rec)
		}
		return err
	})
	return out, err
}

// Credits refreshes and persists the credit wallets.
func (s *Service) Credits(ctx context.Context, acc *pool.Account) (*raccoon.Usage, error) {
	usage, rec, err := useCredential(ctx, s, acc, func(rec *accounts.Record) (*raccoon.Usage, error) {
		return s.client().Balance(ctx, raccoon.CredentialOf(rec))
	})
	if err != nil {
		return nil, err
	}
	if err := s.save(acc, rec, func(latest *accounts.Record) *accounts.Record {
		return withCredits(latest, usage)
	}); err != nil {
		return nil, err
	}
	if usage.Available > 0 {
		acc.ReviveBalanceCooldown()
	}
	return usage, nil
}

// SyncNickname updates only the profile name, preserving operator settings
// and the credential renewed while the request was in flight.
func (s *Service) SyncNickname(ctx context.Context, acc *pool.Account) (string, error) {
	profile, rec, err := useCredential(ctx, s, acc, func(rec *accounts.Record) (map[string]any, error) {
		return s.client().UserInfo(ctx, raccoon.CredentialOf(rec).Token)
	})
	if err != nil {
		return "", err
	}
	name := accounts.RaccoonStringOf(profile, "nickname", "nickName", "name", "displayName", "display_name")
	if name == "" {
		return "", errors.New("小浣熊账号资料未返回昵称")
	}
	err = s.save(acc, rec, func(latest *accounts.Record) *accounts.Record {
		next := *latest
		next.Nickname = name
		return &next
	})
	return name, err
}

func (s *Service) save(acc *pool.Account, observed *accounts.Record, apply func(*accounts.Record) *accounts.Record) error {
	return pool.SaveRaccoonUpdate(s.Pool, acc, observed, s.Persist, apply)
}

// ---------------------------------------------------------------- record shape

func withModels(rec *accounts.Record, models any, at time.Time, errMsg string) *accounts.Record {
	return withModelsAndVisibility(rec, models, nil, nil, at, errMsg)
}

// withModelsAndVisibility stores both the full catalogue and the subset the
// upstream marks visible. Chat may address a hidden Raccoon-Work id that is
// present in the account's full catalogue.
func withModelsAndVisibility(rec *accounts.Record, models, visible any, meta []any, at time.Time, errMsg string) *accounts.Record {
	if models == nil {
		models = []string{}
	}
	return rec.WithRaccoonFields(func(fields map[string]any) {
		fields["models"] = models
		if visible != nil {
			fields["models_visible"] = visible
		}
		if meta != nil {
			fields["models_meta"] = meta
		}
		fields["models_at"] = at.Unix()
		if errMsg != "" {
			fields["models_error"] = errMsg
		} else {
			delete(fields, "models_error")
		}
	})
}

func withCheckin(rec *accounts.Record, res raccoon.CheckinResult, now time.Time) *accounts.Record {
	var points int64
	for _, g := range res.Grants {
		points += g.Points
	}
	next := *rec
	next.LastCheckin = map[string]any{
		"at":      now.In(raccoon.Beijing).Format("2006-01-02 15:04:05"),
		"ok":      res.Success,
		"message": res.Message,
		// credits is a NUMBER: the panel's credit() formats it for every
		// channel, and a "300 积分" string crashes the drawer (toFixed).
		"credits": float64(points),
	}
	return &next
}

func checkinRecorded(rec *accounts.Record, now time.Time) bool {
	if rec == nil {
		return false
	}
	stamp, _ := rec.LastCheckin.(map[string]any)
	ok, _ := stamp["ok"].(bool)
	at, _ := stamp["at"].(string)
	return ok && len(at) >= 10 && at[:10] == raccoon.TodayKey(now)
}

func withCredits(rec *accounts.Record, usage *raccoon.Usage) *accounts.Record {
	if usage == nil {
		return rec
	}
	wallets := make([]any, 0, len(usage.Wallets))
	for _, w := range usage.Wallets {
		wallets = append(wallets, map[string]any{
			"type": w.Type, "displayName": w.Name, "balance": w.Balance,
		})
	}
	view := map[string]any{
		"available": usage.Available,
		// remain/updatedAt are the generic keys the panel's credit column reads
		// for every channel; the wallet detail rides along for the drawer.
		"remain":     usage.Available,
		"updatedAt":  usage.At.Unix(),
		"updated_at": usage.At.Unix(),
		"unit":       usage.Unit,
		"wallets":    wallets,
		"at":         usage.At.Unix(),
		"source":     "raccoon",
	}
	if usage.Subscription != nil {
		view["subscription"] = map[string]any{
			"planName": usage.Subscription.PlanName,
			"status":   usage.Subscription.Status,
			"expireAt": usage.Subscription.ExpireAt,
		}
	}
	if usage.SubscriptionError != "" {
		view["subscriptionError"] = usage.SubscriptionError
	}
	next := *rec
	next.Credits = view
	return &next
}

func withClaims(rec *accounts.Record, ledger map[string]int64, now time.Time) *accounts.Record {
	encoded := make(map[string]any, len(ledger))
	var latest int64
	for k, v := range ledger {
		encoded[k] = v
		if v > latest {
			latest = v
		}
	}
	return rec.WithRaccoonFields(func(fields map[string]any) {
		fields["claims"] = encoded
		if latest > 0 {
			fields["claims_settled_at"] = now.UnixMilli()
		}
	})
}

func settledOf(rec *accounts.Record) map[string]int64 {
	out := map[string]int64{}
	fields := rec.RaccoonFields()
	raw, ok := fields["claims"].(map[string]any)
	if !ok {
		return out
	}
	for k, v := range raw {
		switch n := v.(type) {
		case float64:
			out[k] = int64(n)
		case int64:
			out[k] = n
		case int:
			out[k] = int64(n)
		case json.Number:
			if i, err := n.Int64(); err == nil {
				out[k] = i
			}
		}
	}
	return out
}

// SettledOf exposes the settlement ledger for the admin API.
func SettledOf(rec *accounts.Record) map[string]int64 { return settledOf(rec) }

// ErrDisabled marks a disabled service.
var ErrDisabled = errors.New("raccoon channel is disabled")
