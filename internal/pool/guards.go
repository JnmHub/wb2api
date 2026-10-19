package pool

import "sync/atomic"

// guards are the daily / reserve limits for one account. They are computed off
// the request path (by the guard scan, from the usage database and settings)
// and only read here as atomics, so a request never touches the database.
type guards struct {
	reserve       atomic.Bool                     // balance at or below the reserve line
	dailyToken    atomic.Bool                     // today's token budget is spent
	creditReached atomic.Bool                     // today's credit budget is spent (paid models stop)
	free          atomic.Pointer[map[string]bool] // free models of the realm (exempt from the credit cap)
	modelBlocked  atomic.Pointer[map[string]bool] // models out of their daily token budget
}

func (g *guards) creditBlocked(model string) bool {
	if model == "" || !g.creditReached.Load() {
		return false
	}
	if f := g.free.Load(); f != nil && (*f)[model] {
		return false
	}
	return true
}

func (g *guards) modelTokenBlocked(model string) bool {
	m := g.modelBlocked.Load()
	return m != nil && (*m)[model]
}

// GuardState is one account's computed limits.
type GuardState struct {
	Reserve       bool
	DailyToken    bool
	CreditReached bool
	Free          map[string]bool
	ModelBlocked  map[string]bool
}

// SetGuards installs freshly computed limits and reports whether anything
// changed (so the caller can log or announce it).
func (a *Account) SetGuards(s GuardState) (changed bool) {
	changed = a.guard.reserve.Swap(s.Reserve) != s.Reserve
	changed = (a.guard.dailyToken.Swap(s.DailyToken) != s.DailyToken) || changed
	changed = (a.guard.creditReached.Swap(s.CreditReached) != s.CreditReached) || changed
	a.guard.free.Store(&s.Free)
	old := a.guard.modelBlocked.Swap(&s.ModelBlocked)
	if len(s.ModelBlocked) != lenOf(old) {
		changed = true
	}
	if changed {
		a.touch()
	}
	return changed
}

func lenOf(m *map[string]bool) int {
	if m == nil {
		return 0
	}
	return len(*m)
}

// Guards reports the current limit state.
func (a *Account) Guards() (reserve, dailyToken, creditReached bool) {
	return a.guard.reserve.Load(), a.guard.dailyToken.Load(), a.guard.creditReached.Load()
}

// CreditBlocked: today's credit budget is spent and model is a paid one.
func (a *Account) CreditBlocked(model string) bool { return a.guard.creditBlocked(model) }

// ModelTokenBlocked: model is out of its daily token budget on this account.
func (a *Account) ModelTokenBlocked(model string) bool { return a.guard.modelTokenBlocked(model) }
