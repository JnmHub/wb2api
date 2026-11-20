package refresh

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream/raccoon"
)

var ErrRaccoonDisabled = errors.New("小浣熊账号已停用")

const raccoonRefreshInProgress = "refresh_in_progress"

type manualRaccoonKey struct{}

// WithManualRaccoon allows an explicitly selected account maintenance request
// to renew the credential of a disabled account.
func WithManualRaccoon(ctx context.Context, uid string) context.Context {
	return context.WithValue(ctx, manualRaccoonKey{}, uid)
}

func IsManualRaccoon(ctx context.Context, uid string) bool {
	return uid != "" && (ctx.Value(manualRaccoonKey{}) == uid || IsManualCodeArts(ctx, uid))
}

type raccoonMutation struct {
	observed *accounts.Record
	apply    func(*accounts.Record) *accounts.Record
	err      error
}

// refreshRaccoon serializes exchanges per account. Once an exchange starts
// its new refresh token must be saved even if the initiating request cancels.
func (r *Refresher) refreshRaccoon(ctx context.Context, a *pool.Account, observed *accounts.Record, rejected bool) error {
	if r.Pool != nil && r.Pool.Get(a.UID) != a {
		return accounts.ErrDeleted
	}
	if !a.Enabled() && !IsManualRaccoon(ctx, a.UID) {
		return ErrRaccoonDisabled
	}
	current := a.Record()
	if observed == nil {
		observed = current
	}
	if !pool.SameRaccoonCredential(current, observed) {
		return raccoonOutcome(current, r.now())
	}

	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return ErrStopping
	}
	flight := r.inflt[a.UID]
	if flight == nil {
		if r.inflt == nil {
			r.inflt = make(map[string]*call)
		}
		flight = &call{done: make(chan struct{}), observed: observed, raccoonRejected: rejected}
		r.inflt[a.UID] = flight
		go func() {
			err := r.runRaccoonRefresh(context.WithoutCancel(ctx), a, flight.observed, flight.raccoonRejected)
			r.mu.Lock()
			flight.err = err
			delete(r.inflt, a.UID)
			close(flight.done)
			r.mu.Unlock()
		}()
	}
	r.mu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-flight.done:
		if !pool.SameRaccoonCredential(flight.observed, observed) ||
			(IsManualRaccoon(ctx, a.UID) && errors.Is(flight.err, ErrRaccoonDisabled)) {
			return r.refreshRaccoon(ctx, a, observed, rejected)
		}
		if rejected && !flight.raccoonRejected && flight.err != nil &&
			pool.SameRaccoonCredential(a.Record(), observed) && !a.Record().RaccoonNeedsLogin() {
			// A proactive rate-limited attempt may have left the old token
			// eligible. Tell the backoff path that it was actually rejected.
			return r.refreshRaccoon(ctx, a, observed, true)
		}
		return flight.err
	}
}

func (r *Refresher) runRaccoonRefresh(ctx context.Context, a *pool.Account, observed *accounts.Record, rejected bool) error {
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	rec := a.Record()
	if r.LockRaccoon != nil {
		release, err := r.LockRaccoon(ctx, a.UID)
		if err != nil {
			return fmt.Errorf("raccoon refresh coordination: %w", err)
		}
		defer release()
	}
	if r.LoadLatest != nil {
		latest, err := r.LoadLatest(ctx, a.UID)
		if err != nil {
			return fmt.Errorf("raccoon credential reload: %w", err)
		}
		if !pool.SameRaccoonCredential(rec, latest) || rec.RaccoonAuthState() != latest.RaccoonAuthState() {
			if !a.ReplaceRaccoonRecord(r.Pool, rec, latest) {
				return raccoonOutcome(a.Record(), r.now())
			}
		}
		rec = latest
	}

	r.mu.Lock()
	pending, hasPending := r.raccoonPending[a.UID]
	r.mu.Unlock()
	if hasPending {
		if pool.SameRaccoonCredential(rec, pending.observed) {
			return r.finishRaccoon(a, pending)
		}
		r.clearRaccoonPending(a.UID)
	}
	if !pool.SameRaccoonCredential(rec, observed) {
		return raccoonOutcome(a.Record(), r.now())
	}
	if !rec.IsEnabled() && !IsManualRaccoon(ctx, a.UID) {
		return ErrRaccoonDisabled
	}

	now := r.now()
	auth := rec.RaccoonAuthState()
	switch {
	case auth.Login:
		return raccoonOutcome(rec, now)
	case auth.Reason == raccoonRefreshInProgress:
		return r.recordRaccoonFailure(a, rec, rejected, "refresh_outcome_unknown", true, time.Time{})
	case now.Before(auth.RetryAt):
		if rejected && !auth.AccessRejected {
			auth.AccessRejected = true
			if err := pool.SaveRaccoonUpdate(r.Pool, a, rec, r.Persist, func(current *accounts.Record) *accounts.Record {
				return current.WithRaccoonAuth(auth)
			}); err != nil {
				return err
			}
		}
		return &RetryError{At: auth.RetryAt, Cause: errors.New(auth.Reason)}
	}

	cred := raccoon.CredentialOf(rec)
	switch {
	case cred.Token == "":
		return r.recordRaccoonFailure(a, rec, rejected, "missing_access_token", true, time.Time{})
	case cred.RefreshToken == "":
		if rejected || auth.AccessRejected || (!cred.ExpiresAt.IsZero() && !cred.ExpiresAt.After(now)) {
			reason := "missing_refresh_token"
			if cred.IAM {
				reason = "iam_login_expired"
			}
			return r.recordRaccoonFailure(a, rec, rejected, reason, true, time.Time{})
		}
		return raccoon.ErrNoRefreshToken
	}

	if r.Persist != nil {
		intent := auth
		intent.Reason = raccoonRefreshInProgress
		intent.AccessRejected = intent.AccessRejected || rejected
		intent.RetryAt = now.Add(45 * time.Second)
		if err := pool.SaveRaccoonUpdate(r.Pool, a, rec, r.Persist, func(current *accounts.Record) *accounts.Record {
			return current.WithRaccoonAuth(intent)
		}); err != nil {
			return fmt.Errorf("raccoon refresh intent could not be saved: %w", err)
		}
	}
	if !pool.SameRaccoonCredential(a.Record(), rec) || (r.Pool != nil && r.Pool.Get(a.UID) != a) {
		return raccoonOutcome(a.Record(), now)
	}
	if !a.Enabled() && !IsManualRaccoon(ctx, a.UID) {
		return r.finishRaccoon(a, raccoonMutation{observed: rec, err: ErrRaccoonDisabled,
			apply: func(current *accounts.Record) *accounts.Record { return current.WithRaccoonAuth(auth) }})
	}

	callCtx, stop := context.WithTimeout(ctx, 25*time.Second)
	defer stop()
	client := raccoon.New(&http.Client{Timeout: 25 * time.Second})
	if r.BaseURL != nil {
		client.AuthBase = r.BaseURL(a)
	}
	next, err := client.Refresh(callCtx, cred)
	if err != nil {
		var limited *raccoon.RefreshRateLimit
		switch {
		case errors.As(err, &limited):
			return r.recordRaccoonFailure(a, rec, rejected, "refresh_rate_limited", false, raccoonRetryAfter(limited.RetryAfter, r.now()))
		case errors.Is(err, raccoon.ErrNeedsLogin), errors.Is(err, raccoon.ErrNoRefreshToken), errors.Is(err, raccoon.ErrNoCredential):
			return r.recordRaccoonFailure(a, rec, rejected, "refresh_token_rejected", true, time.Time{})
		default:
			return r.recordRaccoonFailure(a, rec, rejected, "refresh_request_failed", false, time.Time{})
		}
	}
	if err := r.finishRaccoon(a, raccoonMutation{observed: rec,
		apply: func(current *accounts.Record) *accounts.Record { return raccoon.ApplyToken(current, next) }}); err != nil {
		return err
	}
	r.logger().Info("raccoon token refreshed", "account", short(a.UID))
	return nil
}

func (r *Refresher) recordRaccoonFailure(a *pool.Account, rec *accounts.Record, rejected bool, reason string, login bool, retryAfter time.Time) error {
	auth := rec.RaccoonAuthState()
	auth.Reason, auth.Login = reason, login
	auth.AccessRejected = auth.AccessRejected || rejected
	auth.Failures++
	auth.RetryAt = time.Time{}
	err := error(fmt.Errorf("%w (%s)", ErrNeedsLogin, reason))
	if !login {
		auth.RetryAt = r.now().Add(r.backoff(auth.Failures))
		if retryAfter.After(auth.RetryAt) {
			auth.RetryAt = retryAfter
		}
		err = &RetryError{At: auth.RetryAt, Cause: errors.New(reason)}
	}
	return r.finishRaccoon(a, raccoonMutation{
		observed: rec,
		err:      err,
		apply:    func(current *accounts.Record) *accounts.Record { return current.WithRaccoonAuth(auth) },
	})
}

func (r *Refresher) finishRaccoon(a *pool.Account, mutation raccoonMutation) error {
	err := pool.SaveRaccoonUpdate(r.Pool, a, mutation.observed, r.Persist, mutation.apply)
	if err == nil {
		r.clearRaccoonPending(a.UID)
		return mutation.err
	}
	deleted := errors.Is(err, accounts.ErrDeleted) || (r.Pool != nil && r.Pool.Get(a.UID) != a)
	if deleted || !pool.SameRaccoonCredential(a.Record(), mutation.observed) {
		r.clearRaccoonPending(a.UID)
		if deleted {
			return accounts.ErrDeleted
		}
		return raccoonOutcome(a.Record(), r.now())
	}
	// Retain the returned rotating token until persistence can be retried.
	// The next caller replays this write, never the already sent exchange.
	r.mu.Lock()
	if r.raccoonPending == nil {
		r.raccoonPending = make(map[string]raccoonMutation)
	}
	r.raccoonPending[a.UID] = mutation
	r.mu.Unlock()
	return fmt.Errorf("raccoon refresh result could not be saved: %w", err)
}

func (r *Refresher) clearRaccoonPending(uid string) {
	r.mu.Lock()
	delete(r.raccoonPending, uid)
	r.mu.Unlock()
}

func raccoonOutcome(rec *accounts.Record, now time.Time) error {
	auth := rec.RaccoonAuthState()
	switch {
	case rec == nil || auth.Login:
		return fmt.Errorf("%w (%s)", ErrNeedsLogin, auth.Reason)
	case now.Before(auth.RetryAt):
		return &RetryError{At: auth.RetryAt, Cause: errors.New(auth.Reason)}
	case raccoon.AccessUsable(rec, now):
		return nil
	default:
		return errors.New("raccoon credential changed during refresh")
	}
}

func raccoonRetryAfter(value string, now time.Time) time.Time {
	value = strings.TrimSpace(value)
	if delay, err := strconv.ParseInt(value, 10, 32); err == nil && delay >= 0 {
		return now.Add(time.Duration(delay) * time.Second)
	}
	at, _ := http.ParseTime(value)
	return at
}
