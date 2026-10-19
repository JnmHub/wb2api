package pool

import (
	"fmt"
	"math"
)

// SoonestExpiringDays is the days until the earliest credit package that still
// has credits left. Packages the upstream never really expires (free plan,
// enterprise quota) are skipped so those accounts do not look permanently
// "about to lapse".
func (a *Account) SoonestExpiringDays() (float64, bool) {
	pkgs, _ := a.Record().Credits["packages"].([]any)
	best, found := 0.0, false
	for _, raw := range pkgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if truthy(m["no_expiry"]) || truthy(m["is_expired"]) {
			continue
		}
		if s, _ := m["package_code"].(string); s == "enterprise" {
			continue
		}
		days, ok1 := toNumber(m["days_left"])
		remain, ok2 := toNumber(m["remain"])
		if !ok1 || !ok2 || days < 0 || remain <= 0 {
			continue
		}
		if !found || days < best {
			best, found = days, true
		}
	}
	return best, found
}

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	}
	if f, ok := toNumber(v); ok {
		return f != 0
	}
	return true
}

// CreditRemain is the cached remaining balance (ok=false when never fetched).
func (a *Account) CreditRemain() (float64, bool) {
	return toNumber(a.Record().Credits["remain"])
}

// ConsumeCredit subtracts what a finished request actually cost from the cached
// balance, so the reserve guard stays honest between the (slow) credit
// refreshes; the next refresh overwrites the estimate with the real number.
// An unknown balance stays unknown.
func (a *Account) ConsumeCredit(amount float64) {
	if amount <= 0 {
		return
	}
	a.updMu.Lock()
	defer a.updMu.Unlock()
	cur := a.Record()
	remain, ok := toNumber(cur.Credits["remain"])
	if !ok {
		return
	}
	used, _ := toNumber(cur.Credits["used"])
	size, _ := toNumber(cur.Credits["size"])
	next := *cur
	cr := make(map[string]any, len(cur.Credits)+2)
	for k, v := range cur.Credits {
		cr[k] = v
	}
	remain = math.Max(0, remain-amount)
	used += amount
	cr["remain"] = round2(remain)
	cr["used"] = round2(used)
	if size > 0 {
		cr["remain_percent"] = fmt.Sprintf("%.1f%%", remain/size*100)
		cr["used_percent"] = fmt.Sprintf("%.1f%%", math.Min(used, size)/size*100)
	}
	next.Credits = cr
	a.rec.Store(&next)
	a.touch()
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
