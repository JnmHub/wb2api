package pool

import (
	"math"
	"math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Page selects one slice of a realm's pool: conversations past the affinity
// length cap rotate over a page instead of pinning to one account or spreading
// over the whole pool.
type Page struct{ Index, Size int }

type realmPrefs struct {
	weighted []*Account     // manual weight > 1
	expiring []*Account     // soonest-expiring credit inside the window
	rank     map[string]int // uid -> rank among the realm's accounts (sorted by uid)
}

type prefSnap struct{ byRealm map[string]*realmPrefs }

// SetExpiringWindow installs the per-realm expiring-credits window (days; 0 =
// off; the "global" key is the fallback). The window is a dispatch preference,
// never a guard.
func (p *Pool) SetExpiringWindow(days map[string]int) {
	cp := make(map[string]int, len(days))
	for k, v := range days {
		cp[k] = v
	}
	p.expWindow.Store(&cp)
	p.prefDirty.Store(true)
}

// MarkPrefsDirty asks for the dispatch preferences to be recomputed (after a
// credits refresh, say).
func (p *Pool) MarkPrefsDirty() { p.prefDirty.Store(true) }

// ApplyRemainingWeights installs the remaining-usage dispatch weights:
// model -> uid -> weight. Only weights above 1 take part; an empty or nil map
// removes the preference and restores plain rotation.
func (p *Pool) ApplyRemainingWeights(w map[string]map[string]int) {
	if len(w) == 0 {
		if cur := p.remainW.Load(); cur == nil || len(*cur) == 0 {
			return // already clear: the off path must stay allocation-free
		}
	}
	cp := make(map[string]map[string]int, len(w))
	for model, byUID := range w {
		inner := make(map[string]int, len(byUID))
		for uid, weight := range byUID {
			if weight > 1 {
				inner[uid] = weight
			}
		}
		if len(inner) > 0 {
			cp[model] = inner
		}
	}
	p.remainW.Store(&cp)
}

func (p *Pool) remaining(model string) map[string]int {
	if w := p.remainW.Load(); w != nil {
		return (*w)[model]
	}
	return nil
}

// Affinity returns the session-affinity table.
func (p *Pool) Affinity() *SessionAffinity { return p.affinity }

func (p *Pool) window(realm string) int {
	if m := p.expWindow.Load(); m != nil {
		if v, ok := (*m)[realm]; ok {
			return v
		}
		return (*m)["global"]
	}
	return 0
}

func (p *Pool) preferences() *prefSnap {
	if !p.prefDirty.Load() {
		if ps := p.prefs.Load(); ps != nil {
			return ps
		}
	}
	// recompute; concurrent rebuilds are harmless (same inputs)
	p.prefDirty.Store(false)
	snap := p.snap.Load()
	ps := &prefSnap{byRealm: map[string]*realmPrefs{}}
	for realm, list := range snap.byRealm {
		rp := &realmPrefs{rank: make(map[string]int, len(list))}
		uids := make([]string, 0, len(list))
		window := p.window(realm)
		for _, a := range list {
			uids = append(uids, a.UID)
			if a.Weight() > 1 {
				rp.weighted = append(rp.weighted, a)
			}
			if window > 0 {
				if days, ok := a.SoonestExpiringDays(); ok && days <= float64(window) {
					rp.expiring = append(rp.expiring, a)
				}
			}
		}
		sort.Strings(uids)
		for i, u := range uids {
			rp.rank[u] = i
		}
		ps.byRealm[realm] = rp
	}
	p.prefs.Store(ps)
	return ps
}

// smoothPick is smooth weighted round-robin (the nginx algorithm): every entry
// accrues its weight each pick, the largest running total wins and pays back
// the round's total. Equal weights degenerate to a plain rotation.
func smoothPick(mu *sync.Mutex, state map[string]int, entries []*Account, weight func(*Account) int) *Account {
	mu.Lock()
	defer mu.Unlock()
	live := make(map[string]bool, len(entries))
	for _, a := range entries {
		live[a.UID] = true
	}
	for k := range state {
		if !live[k] {
			delete(state, k)
		}
	}
	total := 0
	var best *Account
	for _, a := range entries {
		w := weight(a)
		state[a.UID] += w
		total += w
		if best == nil || state[a.UID] > state[best.UID] {
			best = a
		}
	}
	if best != nil {
		state[best.UID] -= total
	}
	return best
}

func readyOf(list []*Account, exclude map[string]struct{}, model string, now time.Time) []*Account {
	var out []*Account
	for _, a := range list {
		if _, skip := exclude[a.UID]; skip {
			continue
		}
		if a.Ready(model, now) {
			out = append(out, a)
		}
	}
	return out
}

func expiryWeight(a *Account, window int) int {
	days, ok := a.SoonestExpiringDays()
	if !ok {
		return 1
	}
	w := int(math.RoundToEven(float64(window)-days)) + 1
	if w < 1 {
		w = 1
	}
	return w
}

type pageKey struct {
	realm string
	a, b  int
}

func (p *Pool) pageCursor(realm string, pg Page) *atomic.Uint64 {
	key := pageKey{realm, pg.Index, pg.Size}
	if c, ok := p.pageCur.Load(key); ok {
		return c.(*atomic.Uint64)
	}
	c, _ := p.pageCur.LoadOrStore(key, new(atomic.Uint64))
	return c.(*atomic.Uint64)
}

// Pick returns the next ready account of realm for model, skipping exclude.
//
// Preference order: manual weights (>1) first, then accounts whose credits are
// about to lapse, then plain rotation from a shared cursor. A preference only
// reorders who is offered: an account it covers is still skipped when cooling,
// over a limit or excluded, and when it covers nobody the pool falls back to
// the rotation, so one preference can never wedge a realm. On a pool with no
// preferences set this inspects one or two accounts regardless of its size.
func (p *Pool) Pick(realm, model string, exclude map[string]struct{}) *Account {
	return p.pick(realm, model, exclude, nil)
}

func (p *Pool) pick(realm, model string, exclude map[string]struct{}, page *Page) *Account {
	list := p.snap.Load().byRealm[realm]
	if len(list) == 0 {
		return nil
	}
	now := time.Now()
	prefs := p.preferences().byRealm[realm]
	filter := func(src []*Account) []*Account { return src }
	if page != nil {
		if page.Size <= 1 || page.Size >= len(list) {
			page = nil // a page as large as the realm is the whole pool
		} else {
			pg := *page
			filter = func(src []*Account) []*Account {
				var out []*Account
				for _, a := range src {
					if r, ok := prefs.rank[a.UID]; ok && r%pg.Size == pg.Index%pg.Size {
						out = append(out, a)
					}
				}
				return out
			}
		}
	}
	if prefs != nil {
		if len(prefs.weighted) > 0 {
			if ready := readyOf(filter(prefs.weighted), exclude, model, now); len(ready) > 0 {
				return smoothPick(&p.wrrMu, p.wrrManual, ready, func(a *Account) int { return a.Weight() })
			}
		}
		if len(prefs.expiring) > 0 {
			if ready := readyOf(filter(prefs.expiring), exclude, model, now); len(ready) > 0 {
				w := p.window(realm)
				return smoothPick(&p.wrrMu, p.wrrExpire, ready, func(a *Account) int { return expiryWeight(a, w) })
			}
		}
	}
	// Remaining-usage priority (opt-in): accounts close to their estimated
	// window limit take the traffic first, so the quota is spent before it
	// resets. It is a preference like the others: nobody ready falls through.
	if w := p.remaining(model); len(w) > 0 {
		var cand []*Account
		for _, a := range filter(list) {
			if w[a.UID] > 1 {
				cand = append(cand, a)
			}
		}
		if ready := readyOf(cand, exclude, model, now); len(ready) > 0 {
			return smoothPick(&p.wrrMu, p.wrrRemain, ready, func(a *Account) int { return w[a.UID] })
		}
	}
	group := list
	cur := p.cursor(realm)
	if page != nil {
		group = filter(list)
		cur = p.pageCursor(realm, *page)
	}
	n := len(group)
	if n == 0 {
		return nil
	}
	start := cur.Add(1) - 1
	if k := p.TopK(); k > 1 {
		return p.pickTopK(group, exclude, model, now, start, k)
	}
	// Among ready accounts prefer the least loaded one. Ties keep the cursor
	// order, so an idle pool still rotates exactly as before.
	var best *Account
	bestLoad := 0
	for i := 0; i < n; i++ {
		a := group[int((start+uint64(i))%uint64(n))]
		if _, skip := exclude[a.UID]; skip {
			continue
		}
		if !a.Ready(model, now) {
			continue
		}
		load := a.Inflight()
		if best == nil || load < bestLoad {
			best, bestLoad = a, load
		}
	}
	return best
}

// pickTopK collects the K least-loaded ready accounts in cursor order and picks
// one with a mild preference for the least loaded. The wider window avoids
// herding every new session onto the same account when several accounts are
// equally (or nearly) idle; the loaded tail is never considered.
func (p *Pool) pickTopK(group []*Account, exclude map[string]struct{}, model string, now time.Time, start uint64, k int) *Account {
	if k > len(group) {
		k = len(group)
	}
	type candidate struct {
		a    *Account
		load int
	}
	top := make([]candidate, 0, k)
	for i := 0; i < len(group); i++ {
		a := group[int((start+uint64(i))%uint64(len(group)))]
		if _, skip := exclude[a.UID]; skip {
			continue
		}
		if !a.Ready(model, now) {
			continue
		}
		c := candidate{a: a, load: a.Inflight()}
		pos := len(top)
		for j := range top {
			if c.load < top[j].load {
				pos = j
				break
			}
		}
		if pos >= k {
			continue
		}
		top = append(top, candidate{})
		copy(top[pos+1:], top[pos:])
		top[pos] = c
		if len(top) > k {
			top = top[:k]
		}
	}
	if len(top) == 0 {
		return nil
	}
	if len(top) == 1 {
		return top[0].a
	}
	total := 0
	for _, c := range top {
		total += 1_000_000 / (c.load + 1)
	}
	if total <= 0 {
		return top[0].a
	}
	n := rand.IntN(total)
	for _, c := range top {
		n -= 1_000_000 / (c.load + 1)
		if n < 0 {
			return c.a
		}
	}
	return top[len(top)-1].a
}

// PickForSession picks the account for one turn of a conversation: a bound
// conversation keeps its account; when that one is unusable its other warm
// accounts are tried before the pool's own preferences (they may still hold the
// prefix upstream); page marks a conversation past the affinity length cap,
// which rotates over one page of the pool instead of pinning.
func (p *Pool) PickForSession(realm, sessionKey string, exclude map[string]struct{}, model string, page *Page) *Account {
	return p.PickForSessionBest(realm, sessionKey, exclude, model, page, nil)
}

// PickForSessionBest is PickForSession with a preference among the warm
// accounts of one conversation. better reports whether a is a better choice
// than b; nil keeps the most-recent-first order.
func (p *Pool) PickForSessionBest(realm, sessionKey string, exclude map[string]struct{}, model string, page *Page, better func(a, b *Account) bool) *Account {
	if sessionKey != "" && page == nil {
		now := time.Now()
		var best *Account
		for _, uid := range p.affinity.Recent(sessionKey) {
			if _, skip := exclude[uid]; skip {
				continue
			}
			a := p.Get(uid)
			if a == nil || a.Realm != realm || !a.Ready(model, now) {
				continue
			}
			if best == nil || (better != nil && better(a, best)) {
				best = a
			}
		}
		if best != nil {
			p.affinity.Bind(sessionKey, best.UID)
			return best
		}
	}
	a := p.pick(realm, model, exclude, page)
	if a == nil && page != nil {
		// nobody usable on the page: serving from outside beats not serving
		a = p.pick(realm, model, exclude, nil)
	}
	if a != nil && sessionKey != "" && page == nil {
		p.affinity.Bind(sessionKey, a.UID)
	}
	return a
}

// RealmLen is the number of accounts in a realm.
func (p *Pool) RealmLen(realm string) int { return len(p.snap.Load().byRealm[realm]) }
