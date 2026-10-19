package pool

import (
	"sort"
	"strings"
	"time"

	"wb2api/internal/store/accounts"
)

// OnChange registers fn to be told (with the account uid) whenever an account's
// state changes. fn is called synchronously from the mutating goroutine, so it
// must be cheap and non-blocking (the event bus only marks the uid dirty).
func (p *Pool) OnChange(fn func(uid string)) {
	if fn == nil {
		p.notify.Store(nil)
		return
	}
	p.notify.Store(&fn)
}

func (a *Account) touch() {
	if p := a.owner.Load(); p != nil {
		if fn := p.notify.Load(); fn != nil {
			(*fn)(a.UID)
		}
	}
}

// Add puts a new account into the pool (copy-on-write) and returns it. An
// existing account with the same uid is replaced.
func (p *Pool) Add(rec *accounts.Record) *Account {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	cur := p.snap.Load()
	a := NewAccount(rec)
	list := make([]*Account, 0, len(cur.all)+1)
	replaced := false
	for _, x := range cur.all {
		if x.UID == rec.UID {
			list = append(list, a)
			replaced = true
		} else {
			list = append(list, x)
		}
	}
	if !replaced {
		list = append(list, a)
	}
	p.replaceLocked(list)
	a.touch()
	return a
}

// Remove drops an account from the pool; reports whether it existed.
func (p *Pool) Remove(uid string) bool {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	cur := p.snap.Load()
	list := make([]*Account, 0, len(cur.all))
	found := false
	for _, x := range cur.all {
		if x.UID == uid {
			found = true
			continue
		}
		list = append(list, x)
	}
	if !found {
		return false
	}
	p.replaceLocked(list)
	if fn := p.notify.Load(); fn != nil {
		(*fn)(uid)
	}
	return true
}

// Update applies mutate to a copy of the account's record, installs it and
// returns the new record for persistence. ok is false for an unknown uid.
func (p *Pool) Update(uid string, mutate func(*accounts.Record)) (rec *accounts.Record, ok bool) {
	a := p.Get(uid)
	if a == nil {
		return nil, false
	}
	a.updMu.Lock()
	defer a.updMu.Unlock()
	next := *a.Record()
	mutate(&next)
	a.SetRecord(&next)
	return &next, true
}

// Enabled reports the enabled flag.
func (a *Account) Enabled() bool { return a.enabled.Load() }

// TokenBad reports that the credential needs a re-login.
func (a *Account) TokenBad() bool { return a.tokenBad.Load() }

// ExpiresAt returns the token expiry (unix seconds, 0 = unknown).
func (a *Account) ExpiresAt() int64 { return a.expiresAt.Load() }

// CooldownUntil returns the end of the account-wide cooldown (zero = none).
func (a *Account) CooldownUntil() time.Time {
	ns := a.cooldownTo.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// ModelCooldown is one active per-model cooldown.
type ModelCooldown struct {
	Model string
	Until time.Time
}

// ModelCooldowns lists the cooldowns still running at now, soonest first.
func (a *Account) ModelCooldowns(now time.Time) []ModelCooldown {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []ModelCooldown
	for m, ns := range a.modelCool {
		if ns > now.UnixNano() {
			out = append(out, ModelCooldown{m, time.Unix(0, ns)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Until.Equal(out[j].Until) {
			return out[i].Until.Before(out[j].Until)
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// Status is the single coarse state shown in lists and used for filtering.
type Status string

// Account statuses, in the priority order used by State.
const (
	StatusDisabled   Status = "disabled"
	StatusNeedLogin  Status = "need_login"
	StatusRestricted Status = "restricted"
	StatusCooling    Status = "cooling"
	StatusExpiring   Status = "expiring" // token about to expire; the refresher is on it
	StatusReady      Status = "ready"
)

// State classifies the account at now (model-independent).
func (a *Account) State(now time.Time) Status {
	switch {
	case !a.enabled.Load():
		return StatusDisabled
	case a.tokenBad.Load() || a.Record().AccessToken == "":
		return StatusNeedLogin
	}
	ns := now.UnixNano()
	switch {
	case a.restrictedTo.Load() > ns:
		return StatusRestricted
	case a.cooldownTo.Load() > ns:
		return StatusCooling
	}
	if exp := a.expiresAt.Load(); exp != 0 && exp-now.Unix() <= 120 {
		return StatusExpiring
	}
	return StatusReady
}

// ClearCooldowns lifts the account-wide and per-model cooldowns. A risk-control
// restriction is only lifted when includeRestriction is set (an operator who
// knows it was resolved); the updated record is then returned for persistence.
func (a *Account) ClearCooldowns(includeRestriction bool) *accounts.Record {
	a.cooldownTo.Store(0)
	a.mu.Lock()
	a.modelCool = nil
	a.mu.Unlock()
	var rec *accounts.Record
	if includeRestriction && a.restrictedTo.Load() != 0 {
		a.restrictedTo.Store(0)
		next := *a.Record()
		next.RestrictedUntil = 0
		a.rec.Store(&next)
		rec = &next
	}
	a.touch()
	return rec
}

// FindUnique resolves a pinned-route ref to exactly one account: an exact uid,
// else a unique nickname, else a unique uid prefix. A tier with more than one
// match is ambiguous and reported as such — it never guesses. why is "" on
// success, else "was not found" / "is ambiguous".
func (p *Pool) FindUnique(ref string) (a *Account, why string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, "was not found"
	}
	all := p.snap.Load().all
	tiers := []func(*Account) bool{
		func(x *Account) bool { return x.UID == ref },
		func(x *Account) bool { return strings.TrimSpace(x.Record().Nickname) == ref },
		func(x *Account) bool { return strings.HasPrefix(x.UID, ref) },
	}
	for _, match := range tiers {
		var hits []*Account
		for _, x := range all {
			if match(x) {
				hits = append(hits, x)
			}
		}
		switch len(hits) {
		case 1:
			return hits[0], ""
		case 0:
		default:
			return nil, "is ambiguous"
		}
	}
	return nil, "was not found"
}

// ModelDisabled reports whether the operator denied this model on the account.
func (a *Account) ModelDisabled(model string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return model != "" && a.disabledMd[strings.ToLower(strings.TrimSpace(model))]
}
