// Package refresh keeps account access tokens fresh in the background.
//
// The legacy gateway refreshed lazily from inside the request path
// (Account.ready()): a request that happened to pick an account whose token was
// about to expire blocked on a 30 s HTTP call, and a failing refresh was retried
// by every following request. Here refreshing is a separate concern:
//
//   - a scanner renews tokens before they expire (lead time plus a per-account
//     jitter, bounded concurrency), so the request path never waits;
//   - failures back off exponentially per account;
//   - an authorisation failure (401 / no token returned) marks the account as
//     needing a re-login instead of being retried forever;
//   - concurrent refreshes of one account collapse into one call, because the
//     upstream rotates the refresh token on use.
package refresh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream"
	"wb2api/internal/upstream/identity"
)

// RefreshPath is the token refresh endpoint.
const RefreshPath = "/v2/plugin/auth/token/refresh"

// ErrNeedsLogin means the credential is no longer refreshable.
var ErrNeedsLogin = errors.New("token expired: sign in again")

// Refresher renews tokens.
type Refresher struct {
	Pool *pool.Pool
	Up   *upstream.Manager
	Log  *slog.Logger
	// Persist stores the updated record (nil = memory only).
	Persist func(*accounts.Record) error
	// BaseURL overrides the upstream base per account (tests, mock).
	BaseURL func(*pool.Account) string

	// Lead is how long before expiry a token is renewed (default 15 min).
	Lead time.Duration
	// Tick is the scan interval (default 20 s).
	Tick time.Duration
	// Workers bounds concurrent refreshes (default 4).
	Workers int
	// BackoffBase/BackoffMax shape the retry delay after a failure.
	BackoffBase, BackoffMax time.Duration
	// Now is the clock (tests).
	Now func() time.Time

	mu     sync.Mutex
	inflt  map[string]*call
	states map[string]*state
}

type call struct {
	done chan struct{}
	err  error
}

type state struct {
	fails   int
	next    time.Time
	lastErr string
}

// Status describes the refresh health of one account.
type Status struct {
	Failures int
	NextTry  time.Time
	LastErr  string
}

func (r *Refresher) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// lead is how long before expiry a token is renewed. A lead shorter than the
// scan interval would leave tokens that expire between two scans, so the
// effective lead is always at least two ticks (the upstream moved from a 2 h to
// a 48 h lead for exactly this reason).
func (r *Refresher) lead() time.Duration {
	d := r.Lead
	if d <= 0 {
		d = 15 * time.Minute
	}
	if r.Tick > 0 && d <= r.Tick {
		d = 2 * r.Tick
	}
	return d
}

func (r *Refresher) logger() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// StatusOf returns the refresh state of uid.
func (r *Refresher) StatusOf(uid string) Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.states[uid]; s != nil {
		return Status{Failures: s.fails, NextTry: s.next, LastErr: s.lastErr}
	}
	return Status{}
}

func (r *Refresher) backoff(fails int) time.Duration {
	base, max := r.BackoffBase, r.BackoffMax
	if base <= 0 {
		base = 30 * time.Second
	}
	if max <= 0 {
		max = 30 * time.Minute
	}
	d := base
	for i := 1; i < fails && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d
}

// jitter spreads renewals of different accounts over half of the lead time,
// deterministically per uid, so a pool imported in one go does not refresh in
// one burst.
func (r *Refresher) jitter(uid string) time.Duration {
	h := fnv.New32a()
	h.Write([]byte(uid))
	return time.Duration(float64(r.lead()) / 2 * float64(h.Sum32()%1000) / 1000)
}

// due reports whether the account should be refreshed now.
func (r *Refresher) due(a *pool.Account, now time.Time) bool {
	rec := a.Record()
	if rec == nil || rec.RefreshToken == "" || !rec.IsEnabled() {
		return false
	}
	r.mu.Lock()
	st := r.states[a.UID]
	r.mu.Unlock()
	if st != nil && now.Before(st.next) {
		return false
	}
	if rec.AccessToken == "" {
		return st == nil || st.fails == 0 // missing token: try once, then wait for a re-login
	}
	exp := rec.ExpiresAt
	if exp == 0 {
		return false
	}
	return time.Unix(exp, 0).Sub(now) <= r.lead()+r.jitter(a.UID)
}

// ScanOnce refreshes every due account (bounded concurrency) and waits for the
// round to finish. It returns the number of attempted refreshes.
func (r *Refresher) ScanOnce(ctx context.Context) int {
	now := r.now()
	workers := r.Workers
	if workers <= 0 {
		workers = 4
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	n := 0
	for _, a := range r.Pool.All() {
		if ctx.Err() != nil {
			break
		}
		if !r.due(a, now) {
			continue
		}
		n++
		sem <- struct{}{}
		wg.Add(1)
		go func(a *pool.Account) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := r.Refresh(ctx, a); err != nil {
				r.logger().Warn("token refresh failed", "account", short(a.UID), "err", err)
			}
		}(a)
	}
	wg.Wait()
	return n
}

// Run scans until ctx is cancelled.
func (r *Refresher) Run(ctx context.Context) {
	tick := r.Tick
	if tick <= 0 {
		tick = 20 * time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	r.ScanOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.ScanOnce(ctx)
		}
	}
}

func short(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// Refresh renews one account now. Concurrent calls for the same account share
// one upstream request.
func (r *Refresher) Refresh(ctx context.Context, a *pool.Account) error {
	r.mu.Lock()
	if r.inflt == nil {
		r.inflt = map[string]*call{}
		r.states = map[string]*state{}
	}
	if c, ok := r.inflt[a.UID]; ok {
		r.mu.Unlock()
		select {
		case <-c.done:
			return c.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c := &call{done: make(chan struct{})}
	r.inflt[a.UID] = c
	r.mu.Unlock()

	err := r.doRefresh(ctx, a)

	r.mu.Lock()
	delete(r.inflt, a.UID)
	st := r.states[a.UID]
	if st == nil {
		st = &state{}
		r.states[a.UID] = st
	}
	switch {
	case err == nil:
		*st = state{}
	case errors.Is(err, ErrNeedsLogin):
		st.fails++
		st.lastErr = err.Error()
		st.next = r.now().Add(24 * time.Hour) // wait for a re-login
	default:
		st.fails++
		st.lastErr = err.Error()
		st.next = r.now().Add(r.backoff(st.fails))
	}
	r.mu.Unlock()
	c.err = err
	close(c.done)
	return err
}

func pyTitle(s string) string {
	// Python str.title(): a cased letter is upper-cased unless the previous
	// character was also a letter; every other character (digits, "-") resets.
	b := []byte(s)
	prevLetter := false
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z':
			if !prevLetter {
				b[i] = c - 32
			}
			prevLetter = true
		case c >= 'A' && c <= 'Z':
			if prevLetter {
				b[i] = c + 32
			}
			prevLetter = true
		default:
			prevLetter = false
		}
	}
	return string(b)
}

func (r *Refresher) headers(rec *accounts.Record) identity.Headers {
	cfg := realm.Get(rec.Realm)
	ua := fmt.Sprintf("WorkBuddy/%s", identity.DesktopVersionIntl)
	if rec.Realm == realm.CN {
		ua = fmt.Sprintf("WorkBuddy/%s", identity.DesktopVersionCN)
	}
	src, lang := "plugin", "en-US"
	if rec.Realm == realm.CN {
		src, lang = "workbuddy", "zh-CN"
	}
	domain := rec.Domain
	if domain == "" {
		domain = cfg.Domain
	}
	h := identity.Headers{
		identity.P("Content-Type", "application/json"),
		identity.P("Accept", "application/json, text/plain, */*"),
		identity.P("X-Requested-With", "XMLHttpRequest"),
		identity.P("User-Agent", ua),
		identity.P("Origin", cfg.Origin),
		identity.P("Referer", cfg.Origin+"/"),
		identity.P("X-Refresh-Token", rec.RefreshToken),
		identity.P("X-Auth-Refresh-Source", src),
		identity.P("X-User-Id", rec.UID),
		identity.P("X-Domain", domain),
		identity.P("X-CodeBuddy-Request", "1"),
		identity.P("Accept-Language", lang),
	}
	if rec.EnterpriseID != "" {
		h = append(h, identity.P("X-Enterprise-Id", rec.EnterpriseID))
	}
	// The legacy default path title-cases every header name.
	for i := range h {
		h[i].Name = pyTitle(h[i].Name)
	}
	return h
}

func (r *Refresher) doRefresh(ctx context.Context, a *pool.Account) error {
	rec := a.Record()
	if rec.RefreshToken == "" {
		a.MarkTokenBad(true)
		return fmt.Errorf("%w (no refresh token)", ErrNeedsLogin)
	}
	base := realm.Get(rec.Realm).ChatUpstream
	if r.BaseURL != nil {
		base = r.BaseURL(a)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := r.Up.Wire(a.UID, a.Proxy()).Do(ctx, upstream.Request{
		Method: http.MethodPost, URL: strings.TrimRight(base, "/") + RefreshPath,
		Headers: r.headers(rec), Body: []byte("{}"),
	})
	if err != nil {
		return fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		a.MarkTokenBad(true)
		return fmt.Errorf("%w (HTTP 401)", ErrNeedsLogin)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("refresh HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("refresh response: %w", err)
	}
	type tokens struct {
		AccessToken  string          `json:"accessToken"`
		RefreshToken string          `json:"refreshToken"`
		Data         json.RawMessage `json:"data"`
	}
	var outer tokens
	_ = json.Unmarshal(payload.Data, &outer)
	t := outer
	if len(outer.Data) > 0 && string(outer.Data) != "null" {
		var inner tokens
		if json.Unmarshal(outer.Data, &inner) == nil && (inner.AccessToken != "" || inner.RefreshToken != "") {
			t = inner
		}
	}
	if t.AccessToken == "" {
		a.MarkTokenBad(true)
		return fmt.Errorf("%w (refresh returned no token: %s)", ErrNeedsLogin, payload.Msg)
	}
	next := *rec
	next.AccessToken = t.AccessToken
	if t.RefreshToken != "" {
		next.RefreshToken = t.RefreshToken
	}
	if exp := realm.JWTExp(t.AccessToken); exp != 0 {
		next.ExpiresAt = exp
	}
	a.SetRecord(&next)
	a.MarkTokenBad(false)
	if r.Persist != nil {
		if err := r.Persist(&next); err != nil {
			return fmt.Errorf("token refreshed but not saved: %w", err)
		}
	}
	return nil
}
