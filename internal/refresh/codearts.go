package refresh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream/codearts"
)

const (
	codeArtsInProgress = "refresh_in_progress"
	codeArtsSpacing    = 125 * time.Millisecond
)

var errCodeArtsDisabled = errors.New("codearts account is disabled")

type manualCodeArtsKey struct{}

// WithManualCodeArts allows an explicit account management action to use the
// selected account's credentials while it is disabled.
func WithManualCodeArts(ctx context.Context, uid string) context.Context {
	return context.WithValue(ctx, manualCodeArtsKey{}, uid)
}

// IsManualCodeArts reports whether the caller explicitly selected this account.
func IsManualCodeArts(ctx context.Context, uid string) bool {
	return uid != "" && ctx.Value(manualCodeArtsKey{}) == uid
}

// RetryError gives every caller the earliest time that another exchange may
// be attempted for the observed CodeArts credential.
type RetryError struct {
	At    time.Time
	Cause error
}

func (e *RetryError) Error() string { return e.Cause.Error() }
func (e *RetryError) Unwrap() error { return e.Cause }

func RetryTime(err error) (time.Time, bool) {
	var retry *RetryError
	if errors.As(err, &retry) {
		return retry.At, true
	}
	return time.Time{}, false
}

func sameCodeArtsGeneration(a, b *accounts.Record) bool {
	return a != nil && b != nil && a.Realm == b.Realm && a.UID == b.UID &&
		a.AccessToken == b.AccessToken && a.ExpiresAt == b.ExpiresAt &&
		a.CodeArtsCredentialVersion() == b.CodeArtsCredentialVersion()
}

func matchesState(st *state, rec *accounts.Record) bool {
	return st.version == "" || (rec != nil && realm.IsCodeArts(rec.Realm) && st.version == rec.CodeArtsCredentialVersion())
}

func codeArtsCurrentOutcome(rec *accounts.Record, now time.Time) error {
	auth := rec.CodeArtsAuthState()
	switch {
	case auth.Login:
		return fmt.Errorf("%w (%s)", ErrNeedsLogin, auth.Reason)
	case auth.Reason != codeArtsInProgress && now.Before(auth.RetryAt):
		return &RetryError{At: auth.RetryAt, Cause: errors.New(auth.Reason)}
	case rec.CodeArtsAccessUsable(now):
		return nil
	default:
		return errors.New("codearts credential changed during refresh")
	}
}

type codeArtsMutation struct {
	expected *accounts.Record
	apply    func(*accounts.Record) *accounts.Record
	err      error
}

func (r *Refresher) refreshCodeArts(ctx context.Context, a *pool.Account, observed *accounts.Record) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	manual := IsManualCodeArts(ctx, a.UID)
	rec := a.Record()
	if !sameCodeArtsGeneration(rec, observed) {
		return codeArtsCurrentOutcome(rec, r.now())
	}
	if r.LockCodeArts != nil {
		release, err := r.LockCodeArts(ctx, a.UID)
		if err != nil {
			return fmt.Errorf("codearts refresh coordination: %w", err)
		}
		defer release()
	}
	if r.LoadLatest != nil {
		latest, err := r.LoadLatest(ctx, a.UID)
		if err != nil {
			return fmt.Errorf("codearts credential reload: %w", err)
		}
		r.publishCodeArts(a, rec, func(cur *accounts.Record) *accounts.Record {
			if !sameCodeArtsGeneration(cur, latest) || cur.CodeArtsAuthState() != latest.CodeArtsAuthState() {
				return latest
			}
			return nil
		})
		rec = latest
	}
	r.mu.Lock()
	pending, hasPending := r.pending[a.UID]
	r.mu.Unlock()
	if hasPending {
		if sameCodeArtsGeneration(pending.expected, rec) {
			return r.finishCodeArts(a, rec, pending)
		}
		r.clearCodeArtsPending(a.UID)
	}
	if !sameCodeArtsGeneration(rec, observed) {
		return codeArtsCurrentOutcome(rec, r.now())
	}
	auth := rec.CodeArtsAuthState()
	if !auth.Login && !auth.AccessRejected && a.CodeArtsAccessRejected() {
		auth.AccessRejected = true
		if auth.Reason == "" {
			auth.Reason = "access_token_rejected"
		}
		r.mu.Lock()
		if state := r.states[a.UID]; state != nil && matchesState(state, rec) && state.next.After(auth.RetryAt) {
			auth.RetryAt = state.next
		}
		r.mu.Unlock()
		if err := r.saveCodeArts(a, rec, func(current *accounts.Record) *accounts.Record {
			return current.WithCodeArtsAuth(auth)
		}); err != nil {
			return fmt.Errorf("codearts access rejection could not be saved: %w", err)
		}
		rec = a.Record()
	}
	if auth.Login || (auth.Reason != codeArtsInProgress && r.now().Before(auth.RetryAt)) {
		return codeArtsCurrentOutcome(rec, r.now())
	}
	if !manual && (!rec.IsEnabled() || !a.Enabled()) {
		return errCodeArtsDisabled
	}
	if auth.Reason == codeArtsInProgress {
		// We hold the account's lock. A previous process may have exchanged
		// the one-use refresh token without recording the returned credentials.
		return r.recordCodeArtsFailure(a, rec, &codearts.RefreshFailure{
			Reason: "refresh_outcome_unknown", NeedsLogin: true,
		})
	}
	if rec.RefreshToken == "" || codearts.CodeVerifierOf(rec) == "" {
		reason := "missing_refresh_token"
		if rec.RefreshToken != "" {
			reason = "missing_pkce_verifier"
		}
		return r.recordCodeArtsFailure(a, rec, &codearts.RefreshFailure{Reason: reason, NeedsLogin: true})
	}
	base := codearts.STSHost
	if r.BaseURL != nil {
		base = r.BaseURL(a)
	}
	client, egress, err := codeArtsClient(base, a.Proxy())
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	if !sameCodeArtsGeneration(a.Record(), rec) {
		return codeArtsCurrentOutcome(a.Record(), r.now())
	}
	if r.Persist != nil {
		mark := func(current *accounts.Record) *accounts.Record {
			return current.WithCodeArtsAuth(accounts.CodeArtsAuth{
				Reason: codeArtsInProgress, Failures: auth.Failures, RetryAt: r.now().Add(45 * time.Second),
				AccessRejected: a.CodeArtsAccessRejected(),
			})
		}
		if err := r.saveCodeArts(a, rec, mark); err != nil {
			return fmt.Errorf("codearts refresh intent could not be saved: %w", err)
		}
	}
	if err := r.waitCodeArtsSTS(ctx, egress); err != nil {
		return r.recordCodeArtsFailure(a, rec, &codearts.RefreshFailure{Reason: "refresh_queue_wait_failed"})
	}
	if !sameCodeArtsGeneration(a.Record(), rec) || (r.Pool != nil && r.Pool.Get(a.UID) != a) {
		return codeArtsCurrentOutcome(a.Record(), r.now())
	}
	if !manual && !a.Enabled() {
		// The intent was persisted but the exchange has not started.
		if err := r.saveCodeArts(a, rec, func(current *accounts.Record) *accounts.Record {
			return current.WithCodeArtsAuth(auth)
		}); err != nil {
			return fmt.Errorf("codearts refresh intent could not be cleared: %w", err)
		}
		return errCodeArtsDisabled
	}
	// The OAuth endpoint takes the exact original client, PKCE verifier and
	// DPoP private key. The rotated RT and STS signing tuple are one write.
	res, err := codearts.RefreshWithClientID(ctx, client, base, codearts.ClientIDOf(rec),
		rec.RefreshToken, codearts.CodeVerifierOf(rec), codearts.DPoPKeyOf(rec))
	if err != nil {
		var failure *codearts.RefreshFailure
		if !errors.As(err, &failure) {
			failure = &codearts.RefreshFailure{Reason: "refresh_request_failed"}
		}
		return r.recordCodeArtsFailure(a, rec, failure)
	}
	if res.Credentials.AccessKeyID == "" || res.Credentials.SecretAccessKey == "" ||
		res.Credentials.SecurityToken == "" || !res.Expiry().After(r.now().Add(accounts.CodeArtsSafetyWindow)) {
		return r.recordCodeArtsFailure(a, rec, &codearts.RefreshFailure{
			Reason: "invalid_refreshed_credentials", NeedsLogin: true,
		})
	}
	if res.RefreshToken == "" || res.RefreshToken == rec.RefreshToken {
		return r.recordCodeArtsFailure(a, rec, &codearts.RefreshFailure{
			Reason: "refresh_token_not_rotated", NeedsLogin: true,
		})
	}
	return r.finishCodeArts(a, rec, codeArtsMutation{
		expected: rec,
		apply:    func(current *accounts.Record) *accounts.Record { return codearts.ApplyToken(current, res) },
	})
}

func (r *Refresher) recordCodeArtsFailure(a *pool.Account, rec *accounts.Record, failure *codearts.RefreshFailure) error {
	failures := rec.CodeArtsAuthState().Failures + 1
	auth := accounts.CodeArtsAuth{Reason: failure.Reason, Login: failure.NeedsLogin, Failures: failures}
	var err error
	if auth.Login {
		err = fmt.Errorf("%w (%s)", ErrNeedsLogin, auth.Reason)
	} else {
		now := r.now()
		auth.RetryAt = now.Add(r.backoff(failures) + r.jitter(a.UID)*r.backoff(failures)/r.lead()/2)
		if failure.RetryAt.After(auth.RetryAt) {
			auth.RetryAt = failure.RetryAt
		}
		err = &RetryError{At: auth.RetryAt, Cause: failure}
	}
	return r.finishCodeArts(a, rec, codeArtsMutation{
		expected: rec, err: err,
		apply: func(current *accounts.Record) *accounts.Record {
			next := auth
			next.AccessRejected = a.CodeArtsAccessRejected()
			return current.WithCodeArtsAuth(next)
		},
	})
}

func (r *Refresher) finishCodeArts(a *pool.Account, rec *accounts.Record, mutation codeArtsMutation) error {
	if !sameCodeArtsGeneration(a.Record(), rec) {
		r.clearCodeArtsPending(a.UID)
		return codeArtsCurrentOutcome(a.Record(), r.now())
	}
	if err := r.saveCodeArts(a, rec, mutation.apply); err != nil {
		r.mu.Lock()
		if r.pending == nil {
			r.pending = make(map[string]codeArtsMutation)
		}
		r.pending[a.UID] = mutation
		r.mu.Unlock()
		return fmt.Errorf("codearts refresh result could not be saved: %w", err)
	}
	r.clearCodeArtsPending(a.UID)
	return mutation.err
}

func (r *Refresher) clearCodeArtsPending(uid string) {
	r.mu.Lock()
	delete(r.pending, uid)
	r.mu.Unlock()
}

func (r *Refresher) saveCodeArts(a *pool.Account, observed *accounts.Record, apply func(*accounts.Record) *accounts.Record) error {
	current := a.Record()
	if !sameCodeArtsGeneration(current, observed) || (r.Pool != nil && r.Pool.Get(a.UID) != a) {
		return errors.New("credential changed before refresh was saved")
	}
	if r.Persist != nil {
		if err := r.Persist(apply(current)); err != nil {
			return err
		}
		// A shared or reconciled file store publishes the committed state.
		// Reapplying the mutation would make that new baseline look dirty.
		if latest := a.Record(); latest.SharedBaseline != nil &&
			!bytes.Equal(latest.SharedBaseline, current.SharedBaseline) {
			return nil
		}
	}
	r.publishCodeArts(a, observed, apply)
	return nil
}

func (r *Refresher) publishCodeArts(a *pool.Account, observed *accounts.Record, apply func(*accounts.Record) *accounts.Record) {
	update := func(current *accounts.Record) bool {
		if !sameCodeArtsGeneration(current, observed) {
			return false
		}
		if next := apply(current); next != nil {
			*current = *next
			return true
		}
		return false
	}
	if r.Pool != nil {
		r.Pool.UpdateIf(a.UID, update)
	} else {
		current := *a.Record()
		if update(&current) {
			a.SetRecord(&current)
		}
	}
}

func codeArtsClient(base, proxy string) (*http.Client, string, error) {
	target, err := url.Parse(base)
	if err != nil || target.Hostname() == "" {
		return nil, "", errors.New("codearts STS endpoint is invalid")
	}
	egress := strings.ToLower(target.Hostname()) + "|direct"
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if proxy != "" {
		if !strings.Contains(proxy, "://") {
			proxy = "http://" + proxy
		}
		u, err := url.Parse(proxy)
		if err != nil || u.Hostname() == "" {
			return nil, "", errors.New("codearts STS proxy is invalid")
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
			transport.Proxy = http.ProxyURL(u)
		default:
			return nil, "", errors.New("codearts STS proxy scheme is unsupported")
		}
		egress = strings.ToLower(target.Hostname()) + "|proxy:" + strings.ToLower(u.Hostname())
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}, egress, nil
}

func (r *Refresher) waitCodeArtsSTS(ctx context.Context, egress string) error {
	if r.WaitCodeArtsSTS != nil {
		return r.WaitCodeArtsSTS(ctx, egress)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.mu.Lock()
		if r.egress == nil {
			r.egress = make(map[string]time.Time)
		}
		now := time.Now()
		at := r.egress[egress]
		if !at.After(now) {
			r.egress[egress] = now.Add(codeArtsSpacing)
			r.mu.Unlock()
			return nil
		}
		r.mu.Unlock()
		if err := waitForSTS(ctx, time.Until(at)); err != nil {
			return err
		}
	}
}

func waitForSTS(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
