// Package guards computes the daily and reserve limits that park accounts:
// the low-credit reserve line, the per-account daily token and credit budgets,
// and the per-model daily token budget. The numbers come from the usage
// database and the settings; the result is pushed into the pool as atomics so
// the request path never touches either.
package guards

import (
	"log/slog"
	"sync"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/store/settings"
	"wb2api/internal/usagedb"
)

// Usage supplies today's usage by (account, model).
type Usage interface {
	DayUsage(day time.Time) ([]usagedb.DayUse, error)
}

// Scanner recomputes the guards.
type Scanner struct {
	Pool     *pool.Pool
	Settings *settings.Store
	Usage    Usage
	// FreeModels returns the free models of a realm (exempt from the credit cap).
	FreeModels func(realm string) map[string]bool
	Log        *slog.Logger
	// TTL bounds how often the usage fold runs (default 5 s).
	TTL time.Duration

	mu       sync.Mutex
	lastScan time.Time
	lastRev  uint64
	counts   *dayCounts
}

type dayCounts struct {
	day     string
	tokens  map[string]int64
	credits map[string]float64
	models  map[string]map[string]int64
}

func (s *Scanner) ttl() time.Duration {
	if s.TTL > 0 {
		return s.TTL
	}
	return 5 * time.Second
}

func (s *Scanner) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Apply recomputes the guards when the TTL has passed, the settings changed or
// force is set. It is cheap to call per request: inside the TTL it is two
// atomic loads and a mutex.
func (s *Scanner) Apply(force bool) {
	rev := s.Settings.Rev()
	s.mu.Lock()
	if !force && rev == s.lastRev && time.Since(s.lastScan) < s.ttl() {
		s.mu.Unlock()
		return
	}
	s.lastScan, s.lastRev = time.Now(), rev
	s.mu.Unlock()
	s.scan()
}

func (s *Scanner) fold() *dayCounts {
	now := time.Now()
	rows, err := s.Usage.DayUsage(now)
	if err != nil {
		s.logger().Warn("daily guards: usage read failed", "err", err)
		return nil // unknown never parks an account
	}
	c := &dayCounts{day: now.Format("2006-01-02"), tokens: map[string]int64{}, credits: map[string]float64{},
		models: map[string]map[string]int64{}}
	for _, r := range rows {
		c.tokens[r.Account] += r.Tokens
		c.credits[r.Account] += r.Credit
		if c.models[r.Account] == nil {
			c.models[r.Account] = map[string]int64{}
		}
		c.models[r.Account][r.Model] += r.Tokens
	}
	return c
}

func (s *Scanner) scan() {
	limits := s.Settings.Limits()
	tokLim, credLim, modLim := limits[settings.LimitDailyToken], limits[settings.LimitDailyCredit], limits[settings.LimitModelDailyToken]
	resLim := limits[settings.LimitReserveCredits]
	wants := func(e settings.LimitEntry) bool {
		return e.Global > 0 || (e.Intl != nil && *e.Intl > 0) || (e.CN != nil && *e.CN > 0)
	}
	var counts *dayCounts
	if s.Usage != nil && (wants(tokLim) || wants(credLim) || wants(modLim)) {
		counts = s.fold()
	}
	free := map[string]map[string]bool{}
	freeOf := func(realm string) map[string]bool {
		if f, ok := free[realm]; ok {
			return f
		}
		var f map[string]bool
		if s.FreeModels != nil {
			f = s.FreeModels(realm)
		}
		free[realm] = f
		return f
	}
	for _, a := range s.Pool.All() {
		realm := a.Realm
		var st pool.GuardState
		if r := resLim.Effective(realm); r > 0 {
			if remain, ok := a.CreditRemain(); ok {
				st.Reserve = int(remain) <= r
			}
		}
		if counts != nil {
			if lim := tokLim.Effective(realm); lim > 0 {
				st.DailyToken = counts.tokens[a.UID] >= int64(lim)
			}
			if lim := credLim.Effective(realm); lim > 0 {
				st.CreditReached = counts.credits[a.UID] >= float64(lim)
				st.Free = freeOf(realm)
			}
			if lim := modLim.Effective(realm); lim > 0 {
				for m, used := range counts.models[a.UID] {
					if used >= int64(lim) {
						if st.ModelBlocked == nil {
							st.ModelBlocked = map[string]bool{}
						}
						st.ModelBlocked[m] = true
					}
				}
			}
		}
		wasReserve, wasTok, wasCred := a.Guards()
		if a.SetGuards(st) {
			switch {
			case st.DailyToken && !wasTok:
				s.logger().Info("account parked: daily token limit reached", "account", short(a.UID))
			case !st.DailyToken && wasTok:
				s.logger().Info("account resumed: daily token limit cleared", "account", short(a.UID))
			case st.CreditReached && !wasCred:
				s.logger().Info("account capped: daily credit limit reached, free models only", "account", short(a.UID))
			case st.Reserve && !wasReserve:
				s.logger().Info("account parked: below the reserve line", "account", short(a.UID))
			}
		}
	}
	// the expiring-credits window is a dispatch preference
	exp := limits[settings.LimitExpiringWindow]
	s.Pool.SetExpiringWindow(map[string]int{"global": exp.Global, "intl": exp.Effective("intl"), "cn": exp.Effective("cn")})
}

func short(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// Blocked reports whether every enabled, credentialed account of a realm is
// parked by the given guard, with the limit that did it (for error messages).
type Blocked struct {
	Kind  string // "token", "credit", "model"
	Limit int
}

// AllBlocked tells whether the whole realm is parked by a daily guard for model.
func (s *Scanner) AllBlocked(realm, model string) (Blocked, bool) {
	limits := s.Settings.Limits()
	var enabled []*pool.Account
	for _, a := range s.Pool.All() {
		if a.Realm == realm && a.Enabled() && a.Record().AccessToken != "" {
			enabled = append(enabled, a)
		}
	}
	if len(enabled) == 0 {
		return Blocked{}, false
	}
	all := func(f func(*pool.Account) bool) bool {
		for _, a := range enabled {
			if !f(a) {
				return false
			}
		}
		return true
	}
	if all(func(a *pool.Account) bool { _, tok, _ := a.Guards(); return tok }) {
		return Blocked{"token", limits[settings.LimitDailyToken].Effective(realm)}, true
	}
	if model != "" && all(func(a *pool.Account) bool { return a.CreditBlocked(model) }) {
		return Blocked{"credit", limits[settings.LimitDailyCredit].Effective(realm)}, true
	}
	if model != "" && all(func(a *pool.Account) bool { return a.ModelTokenBlocked(model) }) {
		return Blocked{"model", limits[settings.LimitModelDailyToken].Effective(realm)}, true
	}
	return Blocked{}, false
}
