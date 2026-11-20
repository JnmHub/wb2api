package raccoonsync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream/raccoon"
)

func raccoonAccount(uid string, exp time.Time) *accounts.Record {
	return &accounts.Record{
		UID: uid, Realm: realm.Raccoon, AccessToken: "t", RefreshToken: "rt",
		ExpiresAt: exp.Unix(), Enabled: true,
		Extra: map[string]any{raccoon.ExtraKey: map[string]any{
			"userId": "7160938", "officeIdentity": "personal", "models": []any{"sn-glm-5-3"},
		}},
	}
}

func TestCanCheckin(t *testing.T) {
	now := time.Now()
	svc := &Service{Pool: pool.New([]*accounts.Record{
		raccoonAccount("rc-1", now.Add(time.Hour)),
		{UID: "cn-1", Realm: realm.CN, AccessToken: "t", RefreshToken: "rt"},
	})}
	if !svc.CanCheckin(svc.Pool.Get("rc-1")) {
		t.Fatal("a healthy raccoon account must be eligible")
	}
	if svc.CanCheckin(svc.Pool.Get("cn-1")) {
		t.Fatal("a WorkBuddy account must never take part in the raccoon task")
	}
	// A rejected credential is out until someone signs in again.
	rejected := raccoonAccount("rc-2", now.Add(time.Hour))
	rejected = rejected.WithRaccoonAuth(accounts.RaccoonAuth{Reason: "401", Login: true})
	svc.Pool.Add(rejected)
	if svc.CanCheckin(svc.Pool.Get("rc-2")) {
		t.Fatal("a rejected credential must be skipped")
	}
}

func TestDiscoverPersistsCatalogue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/web/llm/v2/model_catalog" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer t" {
			t.Fatalf("authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"categories":[{"type":"chat","models":[
			{"name":"sn-glm-5-3","display_name":"GLM-5-3","visible":true},
			{"name":"raccoon-405a1c","display_name":"Raccoon-Work","visible":false}]}]}}`))
	}))
	defer srv.Close()

	now := time.Unix(1794000000, 0)
	rec := raccoonAccount("rc-1", now.Add(time.Hour))
	p := pool.New([]*accounts.Record{rec})
	var saved *accounts.Record
	svc := &Service{
		Pool: p, Persist: func(r *accounts.Record) error { saved = r; return nil },
		Now:    func() time.Time { return now },
		Client: &raccoon.Client{LlmBase: srv.URL + "/api/web/llm/v2", AuthBase: srv.URL, Origin: srv.URL},
	}
	res := svc.Discover(context.Background(), p.Get("rc-1"))
	if res.Err != nil {
		t.Fatalf("discover: %v", res.Err)
	}
	if len(res.Models) != 2 {
		t.Fatalf("models = %v", res.Models)
	}
	if saved == nil {
		t.Fatal("record not persisted")
	}
	fields := saved.RaccoonFields()
	if at, _ := fields["models_at"].(int64); at != now.Unix() {
		t.Fatalf("models_at = %v, want %d", fields["models_at"], now.Unix())
	}
	if got := raccoon.ModelsOfRecord(saved); len(got) != 2 {
		t.Fatalf("stored models = %v", got)
	}
	// The hidden Raccoon-Work id stays catalogued but is not advertised by the
	// gateway; VisibleModels is what the panel advertises.
	if vis := VisibleModels(raccoon.FallbackModels()); len(vis) == 0 {
		t.Fatal("fallback has no visible model")
	}
}

func TestCheckinStoresLedgerVerdict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/web/office/v3/setting_info":
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		case "/api/web/points/v2/bills":
			_, _ = w.Write([]byte(`{"code":0,"data":{"items":[]}}`))
		case "/api/web/points/v1/balance":
			_, _ = w.Write([]byte(`{"code":0,"data":{"total_chargeable_points":4200,"daily_points":100}}`))
		case "/api/web/auth/v1/entitlement_info":
			_, _ = w.Write([]byte(`{"code":0,"data":{"pro_enable":false}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	now := time.Unix(1794000000, 0)
	p := pool.New([]*accounts.Record{raccoonAccount("rc-1", now.Add(time.Hour))})
	var saved *accounts.Record
	svc := &Service{
		Pool: p, Persist: func(r *accounts.Record) error { saved = r; return nil },
		Now:    func() time.Time { return now },
		Client: &raccoon.Client{LlmBase: srv.URL, AuthBase: srv.URL, Origin: srv.URL},
	}
	res, err := svc.Checkin(context.Background(), p.Get("rc-1"))
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if res.Success {
		t.Fatalf("no ledger entry must not report success: %+v", res)
	}
	if saved == nil {
		t.Fatal("record not persisted")
	}
	stamp, _ := saved.LastCheckin.(map[string]any)
	if stamp == nil || stamp["ok"] != false || stamp["message"] == "" {
		t.Fatalf("lastCheckin = %#v", saved.LastCheckin)
	}
	if saved.Credits == nil || saved.Credits["available"] != int64(4200) {
		t.Fatalf("credits not stored: %#v", saved.Credits)
	}
}

func TestClaimSettlesLedger(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"granted":true,"popup":{"points":3000}}}`))
	}))
	defer srv.Close()

	now := time.Unix(1794000000, 0)
	rec := raccoonAccount("rc-1", now.Add(time.Hour))
	rec.Extra[raccoon.ExtraKey].(map[string]any)["claims"] = map[string]any{"desktop_login_grant": float64(1)}
	p := pool.New([]*accounts.Record{rec})
	var saved *accounts.Record
	svc := &Service{
		Pool: p, Persist: func(r *accounts.Record) error { saved = r; return nil },
		Now:    func() time.Time { return now },
		Client: &raccoon.Client{LlmBase: srv.URL, AuthBase: srv.URL, Origin: srv.URL},
	}
	res, err := svc.Claim(context.Background(), p.Get("rc-1"))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if res.Earned != 3000 || len(res.Outcomes) != 1 {
		t.Fatalf("claim result = %+v", res)
	}
	saved2 := saved.RaccoonFields()
	claims, _ := saved2["claims"].(map[string]any)
	if claims == nil || claims["mobile_login_grant"] == nil {
		t.Fatalf("settled ledger = %#v", saved2["claims"])
	}
	switch v := claims["desktop_login_grant"].(type) {
	case int64:
		if v != 1 {
			t.Fatalf("existing settlement was overwritten: %#v", claims)
		}
	case float64:
		if v != 1 {
			t.Fatalf("existing settlement was overwritten: %#v", claims)
		}
	default:
		t.Fatalf("existing settlement was overwritten: %#v", claims)
	}
}

func TestScannerDue(t *testing.T) {
	now := time.Unix(1794000000, 0)
	rec := raccoonAccount("rc-1", now.Add(time.Hour))
	p := pool.New([]*accounts.Record{rec})
	svc := &Service{Pool: p, Now: func() time.Time { return now }}
	sc := &Scanner{Service: svc}
	acc := p.Get("rc-1")
	if !sc.ModelRefreshDue(acc, now) {
		t.Fatal("an undiscovered account is due")
	}
	rec2 := rec.WithRaccoonAuth(accounts.RaccoonAuth{})
	next := withModels(rec2, []string{"sn-glm-5-3"}, now, "")
	p.Update("rc-1", func(r *accounts.Record) { r.Extra = next.Extra })
	if sc.ModelRefreshDue(p.Get("rc-1"), now.Add(time.Minute)) {
		t.Fatal("a fresh catalogue must not be rescanned")
	}
	if !sc.ModelRefreshDue(p.Get("rc-1"), now.Add(DefaultInterval+time.Minute)) {
		t.Fatal("a stale catalogue must be rescanned")
	}
}
