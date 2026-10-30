package api

import (
	"net/http"
	"sort"
	"time"

	"wb2api/internal/pool"
)

// ModelCoolingView is one row of the model-centric cooldown view. The account
// list answers "which models is this account missing"; this endpoint answers the
// question operators actually ask during an incident: "how many accounts are
// short of this model, and why".
type ModelCoolingView struct {
	Model     string         `json:"model"`
	Realm     string         `json:"realm"`
	Cooling   int            `json:"cooling"`
	Available int            `json:"available"`
	Earliest  int64          `json:"earliest_expiry"`
	Latest    int64          `json:"latest_expiry"`
	Reasons   map[string]int `json:"reasons"`
}

type modelCoolAgg struct {
	cooling  int
	earliest time.Time
	latest   time.Time
	reasons  map[string]int
}

// modelCoolingRows is the pure aggregation behind the endpoint, split out so
// the arithmetic (per-model counts, availability, reasons, ordering) is
// testable without a router.
func modelCoolingRows(accs []*pool.Account, now time.Time) ([]ModelCoolingView, map[string]int) {
	rows := map[[2]string]*modelCoolAgg{}
	byRealm := map[string][]*pool.Account{}
	enabled := map[string]int{}
	for _, acc := range accs {
		rec := acc.Record()
		if !rec.IsEnabled() || rec.AccessToken == "" {
			continue
		}
		enabled[rec.Realm]++
		byRealm[rec.Realm] = append(byRealm[rec.Realm], acc)
		for _, one := range acc.ModelCooldowns(now) {
			k := [2]string{rec.Realm, one.Model}
			agg := rows[k]
			if agg == nil {
				agg = &modelCoolAgg{reasons: map[string]int{}}
				rows[k] = agg
			}
			agg.cooling++
			if agg.earliest.IsZero() || one.Until.Before(agg.earliest) {
				agg.earliest = one.Until
			}
			if one.Until.After(agg.latest) {
				agg.latest = one.Until
			}
			reason := one.Reason
			if reason == "" {
				reason = "unknown"
			}
			agg.reasons[reason]++
		}
	}
	out := make([]ModelCoolingView, 0, len(rows))
	for k, agg := range rows {
		free := 0
		for _, acc := range byRealm[k[0]] {
			if !acc.ModelBlocked(k[1], now) {
				free++
			}
		}
		out = append(out, ModelCoolingView{
			Model: k[1], Realm: k[0], Cooling: agg.cooling, Available: free,
			Earliest: agg.earliest.Unix(), Latest: agg.latest.Unix(), Reasons: agg.reasons,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cooling != out[j].Cooling {
			return out[i].Cooling > out[j].Cooling
		}
		if out[i].Realm != out[j].Realm {
			return out[i].Realm < out[j].Realm
		}
		return out[i].Model < out[j].Model
	})
	return out, enabled
}

// modelCooling reports, per (realm, model), how many enabled accounts are in a
// model window right now, how many could still take the model, when the first
// window expires and why the windows are running.
func (a *Accounts) modelCooling(w http.ResponseWriter, r *http.Request) {
	if a.Pool == nil {
		writeJSON(w, http.StatusOK, map[string]any{"models": []ModelCoolingView{}, "enabled": map[string]int{}})
		return
	}
	rows, enabled := modelCoolingRows(a.Pool.All(), a.now())
	writeJSON(w, http.StatusOK, map[string]any{"models": rows, "enabled": enabled})
}
