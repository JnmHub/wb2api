// Package pool holds the live account set and decides which account serves a
// request. Design rules (see docs/REFACTOR_PLAN.md, section 4.2):
//
//   - The account list is an immutable snapshot swapped atomically, so the
//     request path never takes a pool-wide lock.
//   - Readiness checks only read atomics. They NEVER perform network IO: token
//     refresh is a background concern (refresher), so one expiring credential
//     can no longer stall an unrelated request.
//   - Selection does not walk the whole pool on the happy path.
package pool

import (
	"sync"
	"sync/atomic"
	"time"

	"wb2api/internal/store/accounts"
)

// Account is the live view of one credential: the persisted record plus
// runtime state (cooldowns, in-flight count, token health).
type Account struct {
	UID   string
	Realm string

	rec atomic.Pointer[accounts.Record] // replaced wholesale when credentials change

	enabled      atomic.Bool
	tokenBad     atomic.Bool  // no usable token (expired and refresh failed / missing)
	expiresAt    atomic.Int64 // unix seconds
	cooldownTo   atomic.Int64 // unix nanoseconds; account-wide cooldown
	restrictedTo atomic.Int64 // unix nanoseconds; upstream risk-control hold
	inflight     atomic.Int32
	maxInflight  atomic.Int32 // 0 = unlimited
	weight       atomic.Int32

	mu         sync.Mutex
	modelCool  map[string]int64 // model -> unix nanoseconds
	disabledMd map[string]bool  // manual per-model deny list (lower-case)
	stats      Stats
}

// Stats are cheap counters shown on the panel.
type Stats struct {
	Requests int64
	Failures int64
}

// NewAccount wraps a persisted record.
func NewAccount(rec *accounts.Record) *Account {
	a := &Account{UID: rec.UID, Realm: rec.Realm}
	a.SetRecord(rec)
	return a
}

// SetRecord installs a (new) persisted record and refreshes derived flags.
func (a *Account) SetRecord(rec *accounts.Record) {
	a.rec.Store(rec)
	a.enabled.Store(rec.IsEnabled())
	a.expiresAt.Store(rec.ExpiresAt)
	a.tokenBad.Store(rec.AccessToken == "")
	a.weight.Store(int32(rec.Weight))
	a.restrictedTo.Store(int64(rec.RestrictedUntil * 1e9))
	dm := make(map[string]bool, len(rec.DisabledModels))
	for _, m := range rec.DisabledModels {
		dm[m] = true
	}
	a.mu.Lock()
	a.disabledMd = dm
	a.mu.Unlock()
}

// Record returns the current persisted record (treat as read-only).
func (a *Account) Record() *accounts.Record { return a.rec.Load() }

// Weight returns the manual scheduling weight (>=1).
func (a *Account) Weight() int { return int(a.weight.Load()) }

// Inflight returns the number of requests currently using the account.
func (a *Account) Inflight() int { return int(a.inflight.Load()) }

// SetMaxInflight caps concurrent requests per account (0 = unlimited).
func (a *Account) SetMaxInflight(n int) { a.maxInflight.Store(int32(n)) }

// Ready reports whether the account may serve model right now. It reads only
// in-memory state. now is passed in so a scan can reuse one clock reading.
func (a *Account) Ready(model string, now time.Time) bool {
	if !a.enabled.Load() || a.tokenBad.Load() {
		return false
	}
	ns := now.UnixNano()
	if a.cooldownTo.Load() > ns || a.restrictedTo.Load() > ns {
		return false
	}
	if max := a.maxInflight.Load(); max > 0 && a.inflight.Load() >= max {
		return false
	}
	if model != "" {
		a.mu.Lock()
		blocked := a.disabledMd[model]
		until := a.modelCool[model]
		a.mu.Unlock()
		if blocked || until > ns {
			return false
		}
	}
	// A token inside its last 120 seconds is treated as unusable until the
	// background refresher replaces it; the request path never refreshes.
	if exp := a.expiresAt.Load(); exp != 0 && exp-now.Unix() <= 120 {
		return false
	}
	return true
}

// Acquire marks one request in flight; the caller must call Release.
func (a *Account) Acquire() { a.inflight.Add(1) }

// Release ends an Acquire.
func (a *Account) Release() { a.inflight.Add(-1) }

// CoolDown parks the whole account until t.
func (a *Account) CoolDown(until time.Time) { a.cooldownTo.Store(until.UnixNano()) }

// CoolDownModel parks one model on this account until t.
func (a *Account) CoolDownModel(model string, until time.Time) {
	a.mu.Lock()
	if a.modelCool == nil {
		a.modelCool = map[string]int64{}
	}
	a.modelCool[model] = until.UnixNano()
	a.mu.Unlock()
}

// MarkTokenBad flags the credential as unusable (needs re-login or refresh).
func (a *Account) MarkTokenBad(bad bool) { a.tokenBad.Store(bad) }

// SetExpiresAt records a refreshed token expiry (unix seconds).
func (a *Account) SetExpiresAt(sec int64) { a.expiresAt.Store(sec) }

// SetEnabled toggles the account.
func (a *Account) SetEnabled(on bool) { a.enabled.Store(on) }

// NoteResult updates the counters.
func (a *Account) NoteResult(ok bool) {
	a.mu.Lock()
	a.stats.Requests++
	if !ok {
		a.stats.Failures++
	}
	a.mu.Unlock()
}

// Snapshot returns the counters.
func (a *Account) Snapshot() Stats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stats
}

type snapshot struct {
	all     []*Account
	byRealm map[string][]*Account
	byUID   map[string]*Account
}

// Pool is the set of live accounts.
type Pool struct {
	snap    atomic.Pointer[snapshot]
	cursors sync.Map // realm -> *atomic.Uint64
}

// New builds a pool from records.
func New(recs []*accounts.Record) *Pool {
	p := &Pool{}
	list := make([]*Account, 0, len(recs))
	for _, r := range recs {
		list = append(list, NewAccount(r))
	}
	p.Replace(list)
	return p
}

// Replace swaps in a new account list (copy-on-write).
func (p *Pool) Replace(list []*Account) {
	s := &snapshot{
		all:     list,
		byRealm: map[string][]*Account{},
		byUID:   make(map[string]*Account, len(list)),
	}
	for _, a := range list {
		s.byRealm[a.Realm] = append(s.byRealm[a.Realm], a)
		s.byUID[a.UID] = a
	}
	p.snap.Store(s)
}

// Len returns the number of accounts.
func (p *Pool) Len() int { return len(p.snap.Load().all) }

// Get returns an account by uid.
func (p *Pool) Get(uid string) *Account { return p.snap.Load().byUID[uid] }

// All returns the current snapshot slice (read-only).
func (p *Pool) All() []*Account { return p.snap.Load().all }

func (p *Pool) cursor(realm string) *atomic.Uint64 {
	if c, ok := p.cursors.Load(realm); ok {
		return c.(*atomic.Uint64)
	}
	c, _ := p.cursors.LoadOrStore(realm, new(atomic.Uint64))
	return c.(*atomic.Uint64)
}

// Pick returns the next ready account of realm for model, skipping exclude.
// It scans from a shared rotating cursor, so on a healthy pool it inspects one
// or two accounts regardless of pool size. It returns nil when nobody is ready.
func (p *Pool) Pick(realm, model string, exclude map[string]struct{}) *Account {
	list := p.snap.Load().byRealm[realm]
	n := len(list)
	if n == 0 {
		return nil
	}
	now := time.Now()
	start := p.cursor(realm).Add(1) - 1
	for i := 0; i < n; i++ {
		a := list[int((start+uint64(i))%uint64(n))]
		if _, skip := exclude[a.UID]; skip {
			continue
		}
		if a.Ready(model, now) {
			return a
		}
	}
	return nil
}

// ReadyCount counts ready accounts (used for retry budgeting and /health). It
// is O(n); never call it per request.
func (p *Pool) ReadyCount(realm, model string) int {
	now := time.Now()
	c := 0
	for _, a := range p.snap.Load().byRealm[realm] {
		if a.Ready(model, now) {
			c++
		}
	}
	return c
}
