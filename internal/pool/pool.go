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
	"bytes"
	"encoding/json"
	"reflect"
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
	breakerTo    atomicInt64  // unix nanoseconds; breaker after consecutive hard failures
	degradeTo    atomicInt64  // unix nanoseconds; degrade window after unknown failures
	balanceTo    atomicInt64  // unix nanoseconds; 402 park until the daily reset
	guard        guards       // daily / reserve limits, refreshed by the guard scan
	inflight     atomic.Int32
	maxInflight  atomic.Int32 // 0 = unlimited
	weight       atomic.Int32

	owner atomic.Pointer[Pool] // set when the account joins a pool; used to announce changes
	updMu sync.Mutex           // serialises read-modify-write of the record (Pool.Update)

	mu         sync.Mutex
	modelCool  map[string]int64  // model -> unix nanoseconds
	modelWhy   map[string]string // model -> reason for the window (diagnostics)
	g          governState
	disabledMd map[string]bool // manual per-model deny list (lower-case)
	stats      Stats
}

type atomicInt64 = atomic.Int64

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
	a.restoreCaps(rec, time.Now())
	if p := a.owner.Load(); p != nil {
		p.prefDirty.Store(true)
	}
	a.touch()
}

// Record returns the current persisted record (treat as read-only).
func (a *Account) Record() *accounts.Record { return a.rec.Load() }

// Weight returns the manual scheduling weight (>=1).
func (a *Account) Weight() int { return int(a.weight.Load()) }

// Inflight returns the number of requests currently using the account.
func (a *Account) Inflight() int { return int(a.inflight.Load()) }

// SetMaxInflight caps concurrent requests per account (0 = unlimited).
func (a *Account) SetMaxInflight(n int) {
	if n < 0 {
		n = 0
	}
	a.maxInflight.Store(int32(n))
}

// MaxInflight returns the configured per-account cap (0 = unlimited).
func (a *Account) MaxInflight() int { return int(a.maxInflight.Load()) }

// AtCapacity reports whether this process has filled the effective account cap.
func (a *Account) AtCapacity(limit int) bool {
	local := int(a.maxInflight.Load())
	if local > 0 && (limit <= 0 || local < limit) {
		limit = local
	}
	return limit > 0 && a.Inflight() >= limit
}

// Ready reports whether the account may serve model right now. It reads only
// in-memory state. now is passed in so a scan can reuse one clock reading.
func (a *Account) Ready(model string, now time.Time) bool {
	return a.ready(model, now, true)
}

// HealthReady reports readiness without considering local slot capacity.
func (a *Account) HealthReady(model string, now time.Time) bool {
	return a.ready(model, now, false)
}

func (a *Account) ready(model string, now time.Time, checkCapacity bool) bool {
	if !a.enabled.Load() || a.tokenBad.Load() {
		return false
	}
	ns := now.UnixNano()
	if a.cooldownTo.Load() > ns || a.restrictedTo.Load() > ns || a.breakerTo.Load() > ns ||
		a.degradeTo.Load() > ns || a.balanceTo.Load() > ns {
		return false
	}
	if a.guard.reserve.Load() || a.guard.dailyToken.Load() {
		return false
	}
	if checkCapacity && a.AtCapacity(0) {
		return false
	}
	// A known, exhausted balance must not serve any model (including free ones):
	// the account would only produce upstream 402s until the balance is topped
	// up or the daily reset restores it.
	if remain, ok := a.CreditRemain(); ok && remain <= 0 {
		return false
	}
	if model != "" {
		a.mu.Lock()
		blocked := a.disabledMd[model]
		until := a.modelCool[model]
		a.mu.Unlock()
		if blocked || until > ns || a.guard.creditBlocked(model) || a.guard.modelTokenBlocked(model) {
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

// TryAcquire atomically reserves a local slot. limit is the effective shared
// account cap (0 = unlimited); a lower account-specific cap also applies.
// Ready is only a scheduling hint: callers must reserve before starting work
// because other requests may select the same account between checking and dispatch.
func (a *Account) TryAcquire(limit int) (func(), bool) {
	local := int(a.maxInflight.Load())
	if local > 0 && (limit <= 0 || local < limit) {
		limit = local
	}
	for {
		n := a.inflight.Load()
		if limit > 0 && n >= int32(limit) {
			return nil, false
		}
		if a.inflight.CompareAndSwap(n, n+1) {
			var once sync.Once
			return func() { once.Do(a.Release) }, true
		}
	}
}

// Acquire marks one request in flight without checking capacity. Retained for
// callers recording already-reserved work; request dispatch must use TryAcquire.
func (a *Account) Acquire() { a.inflight.Add(1) }

// Release ends an Acquire.
func (a *Account) Release() { a.inflight.Add(-1) }

// CoolDown parks the whole account until t.
func (a *Account) CoolDown(until time.Time) { a.cooldownTo.Store(until.UnixNano()); a.touch() }

// CoolDownModel parks one model on this account until t.
func (a *Account) CoolDownModel(model string, until time.Time) {
	a.CoolDownModelWhy(model, until, "manual")
}

// CoolDownModelWhy is CoolDownModel with an explicit provenance string, used
// for diagnostics and to distinguish a local window from a shared one adopted
// from another replica.
func (a *Account) CoolDownModelWhy(model string, until time.Time, why string) {
	a.mu.Lock()
	if a.modelCool == nil {
		a.modelCool = map[string]int64{}
	}
	a.modelCool[model] = until.UnixNano()
	a.setCoolWhyLocked(model, why)
	a.mu.Unlock()
	a.touch()
}

// setCoolWhyLocked records why the model window runs. Caller holds a.mu.
func (a *Account) setCoolWhyLocked(model, why string) {
	if model == "" || why == "" {
		return
	}
	if a.modelWhy == nil {
		a.modelWhy = map[string]string{}
	}
	a.modelWhy[model] = why
}

// MarkTokenBad flags the credential as unusable (needs re-login or refresh).
func (a *Account) MarkTokenBad(bad bool) { a.tokenBad.Store(bad); a.touch() }

// SetExpiresAt records a refreshed token expiry (unix seconds).
func (a *Account) SetExpiresAt(sec int64) { a.expiresAt.Store(sec); a.touch() }

// SetEnabled toggles the account.
func (a *Account) SetEnabled(on bool) { a.enabled.Store(on); a.touch() }

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
	notify  atomic.Pointer[func(uid string)]
	writeMu sync.Mutex // serialises membership changes (Add / Remove / Replace)

	affinity       *SessionAffinity
	prefs          atomic.Pointer[prefSnap] // dispatch preferences (weights, expiring credits), rebuilt lazily
	prefDirty      atomic.Bool
	expWindow      atomic.Pointer[map[string]int] // realm -> expiring window in days
	wrrMu          sync.Mutex
	wrrManual      map[string]int                            // smooth weighted round-robin state: manual weights
	wrrExpire      map[string]int                            // ... expiring-credits window
	wrrRemain      map[string]int                            // ... remaining-usage estimate
	remainW        atomic.Pointer[map[string]map[string]int] // legacy compatibility, no dispatch reads
	remainSnapshot atomic.Pointer[RemainingSnapshot]         // immutable advisory scheduling weights
	pageCur        sync.Map                                  // pageKey -> *atomic.Uint64
	slots          atomic.Pointer[map[string]string]         // enabled proxy slot id -> URL
	topK           atomic.Int32                              // select among the K least-loaded ready accounts (1 = strict least-loaded)
	scorer         atomic.Pointer[Scorer]                    // optional feedback-aware rank; lower is better
}

// Scorer ranks an account for a model. Lower scores are preferred. It must be
// cheap and allocation-free: selection calls it for a handful of candidates on
// the request path.
type Scorer func(a *Account, model string) int64

// New builds a pool from records.
func New(recs []*accounts.Record) *Pool {
	p := &Pool{affinity: NewSessionAffinity(), wrrManual: map[string]int{}, wrrExpire: map[string]int{}, wrrRemain: map[string]int{}}
	list := make([]*Account, 0, len(recs))
	for _, r := range recs {
		list = append(list, NewAccount(r))
	}
	p.Replace(list)
	return p
}

// Replace swaps in a new account list (copy-on-write).
func (p *Pool) Replace(list []*Account) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	p.replaceLocked(list)
}

func (p *Pool) replaceLocked(list []*Account) {
	for _, a := range list {
		a.owner.Store(p)
	}
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
	p.prefDirty.Store(true)
}

// SyncRecords applies an authoritative shared-store snapshot without replacing
// live Account pointers (and their in-flight/cooldown/statistics state).
func (p *Pool) SyncRecords(recs []*accounts.Record) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	old := p.snap.Load()
	list := make([]*Account, 0, len(recs))
	seen := make(map[string]bool, len(recs))
	for _, rec := range recs {
		seen[rec.UID] = true
		a := old.byUID[rec.UID]
		if a == nil || a.Realm != rec.Realm {
			if a != nil {
				a.enabled.Store(false)
			}
			a = NewAccount(rec)
		} else {
			a.updMu.Lock()
			prev := a.Record()
			// ConsumeCredit keeps a local estimate in the record. An unchanged
			// authoritative credits snapshot must not replenish that estimate at
			// every poll; a genuine refresh (including updated_at) replaces it.
			if sameSharedCredits(prev.SharedBaseline, rec.SharedBaseline) {
				next := *rec
				next.Credits = prev.Credits
				rec = &next
			}
			// Apply only persisted changes; polling must not reset locally learned
			// token health, restriction holds or quota observations.
			a.rec.Store(rec)
			a.enabled.Store(rec.IsEnabled())
			a.expiresAt.Store(rec.ExpiresAt)
			a.weight.Store(int32(rec.Weight))
			if prev.AccessToken != rec.AccessToken {
				a.tokenBad.Store(rec.AccessToken == "")
			}
			if prev.RestrictedUntil != rec.RestrictedUntil {
				a.restrictedTo.Store(int64(rec.RestrictedUntil * 1e9))
			}
			if !reflect.DeepEqual(prev.DisabledModels, rec.DisabledModels) {
				dm := make(map[string]bool, len(rec.DisabledModels))
				for _, model := range rec.DisabledModels {
					dm[model] = true
				}
				a.mu.Lock()
				a.disabledMd = dm
				a.mu.Unlock()
			}
			if !reflect.DeepEqual(prev.ModelCaps, rec.ModelCaps) {
				a.restoreCaps(rec, time.Now())
			}
			a.touch()
			a.updMu.Unlock()
		}
		list = append(list, a)
	}
	for uid, a := range old.byUID {
		if !seen[uid] {
			a.enabled.Store(false)
		}
	}
	p.replaceLocked(list)
}

// sameSharedCredits compares persisted values, not the locally consumed estimate.
// Decode numbers consistently: PostgreSQL jsonb may reorder keys and whitespace.
func sameSharedCredits(before, next []byte) bool {
	if before == nil || next == nil {
		return false
	}
	if bytes.Equal(before, next) {
		return true
	}
	credits := func(raw []byte) (any, bool) {
		var doc map[string]any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil || doc == nil {
			return nil, false
		}
		return doc["credits"], true
	}
	a, okA := credits(before)
	b, okB := credits(next)
	return okA && okB && reflect.DeepEqual(a, b)
}

// Len returns the number of accounts.
func (p *Pool) Len() int { return len(p.snap.Load().all) }

// SetTopK controls how many of the least-loaded ready accounts the fallback
// picker chooses from. 1 keeps the strict least-loaded behavior; larger values
// spread tied or near-tied accounts while still excluding the loaded tail.
func (p *Pool) SetTopK(k int) {
	if k < 1 {
		k = 1
	}
	p.topK.Store(int32(k))
}

// TopK returns the configured top-K width.
func (p *Pool) TopK() int { return int(p.topK.Load()) }

// SetScorer installs the feedback-aware rank used by the pickers. A nil scorer
// restores the default load-only ordering.
func (p *Pool) SetScorer(s Scorer) {
	if s == nil {
		p.scorer.Store(nil)
		return
	}
	p.scorer.Store(&s)
}

// scoreOf returns the rank for one candidate. The default is the in-flight
// count, so a pool without feedback keeps the historical least-loaded order.
func (p *Pool) scoreOf(a *Account, model string) int64 {
	if s := p.scorer.Load(); s != nil {
		return (*s)(a, model)
	}
	return int64(a.Inflight())
}

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

// NoteRestricted parks the account after an upstream risk-control restriction.
// It only starts the clock on the first hit (repeated probing must not push
// the deadline further out) and returns the updated record for persistence, or
// nil when the account was already restricted.
func (a *Account) NoteRestricted(now time.Time, cooldown time.Duration) *accounts.Record {
	if a.restrictedTo.Load() > now.UnixNano() {
		return nil
	}
	until := now.Add(cooldown)
	a.restrictedTo.Store(until.UnixNano())
	rec := *a.Record()
	rec.RestrictedUntil = float64(until.UnixNano()) / 1e9
	a.rec.Store(&rec)
	a.touch()
	return &rec
}

// RestrictedUntil returns the end of the restriction (zero when none).
func (a *Account) RestrictedUntil() time.Time {
	ns := a.restrictedTo.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}
