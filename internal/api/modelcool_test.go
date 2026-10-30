package api

import (
	"testing"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/store/accounts"
)

func TestModelCoolingRowsAggregatesPerRealmAndModel(t *testing.T) {
	exp := time.Now().Add(24 * time.Hour).Unix()
	mk := func(uid, realmName string) *accounts.Record {
		return accounts.FromMap(map[string]any{
			"uid": uid, "accessToken": "tok-" + uid, "realm": realmName, "expiresAt": exp,
		}, "", time.Now())
	}
	p := pool.New([]*accounts.Record{mk("a0", "intl"), mk("a1", "intl"), mk("c0", "cn")})
	now := time.Now()
	const flash = "deepseek-v4.1-flash"
	p.Get("a0").CoolDownModelWhy(flash, now.Add(5*time.Minute), "soft")
	if _, ok := p.Get("a1").NoteModelCap(flash, now.Add(time.Hour), now); !ok {
		t.Fatal("cap window not recorded")
	}
	p.Get("c0").CoolDownModelWhy(flash, now.Add(2*time.Minute), "entitlement")

	rows, enabled := modelCoolingRows(p.All(), now)
	if enabled["intl"] != 2 || enabled["cn"] != 1 {
		t.Fatalf("enabled = %v", enabled)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2: %+v", len(rows), rows)
	}
	// intl has two accounts in a window, so it sorts before cn.
	intl := rows[0]
	if intl.Realm != "intl" || intl.Model != flash {
		t.Fatalf("first row = %+v", intl)
	}
	if intl.Cooling != 2 || intl.Available != 0 {
		t.Fatalf("intl counts = %+v", intl)
	}
	if intl.Reasons["soft"] != 1 || intl.Reasons["cap"] != 1 {
		t.Fatalf("intl reasons = %v", intl.Reasons)
	}
	if !time.Unix(intl.Latest, 0).After(time.Unix(intl.Earliest, 0)) {
		t.Fatalf("expiry window not ordered: %+v", intl)
	}
	cn := rows[1]
	if cn.Realm != "cn" || cn.Cooling != 1 || cn.Reasons["entitlement"] != 1 {
		t.Fatalf("cn row = %+v", cn)
	}
}

func TestModelCoolingRowsSkipsDisabledAccounts(t *testing.T) {
	exp := time.Now().Add(time.Hour).Unix()
	rec := accounts.FromMap(map[string]any{
		"uid": "a0", "accessToken": "t", "realm": "intl", "expiresAt": exp, "enabled": false,
	}, "", time.Now())
	p := pool.New([]*accounts.Record{rec})
	now := time.Now()
	p.Get("a0").CoolDownModelWhy("m", now.Add(time.Minute), "soft")
	rows, enabled := modelCoolingRows(p.All(), now)
	if len(rows) != 0 || len(enabled) != 0 {
		t.Fatalf("disabled account leaked into the view: %+v %v", rows, enabled)
	}
}
