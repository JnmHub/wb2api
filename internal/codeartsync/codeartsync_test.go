package codeartsync

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream/codearts"
)

func syncAccount(uid string) *accounts.Record {
	return &accounts.Record{
		UID: uid, Realm: realm.CodeArts, AccessToken: "STS", RefreshToken: "r",
		Extra: map[string]any{"codearts": map[string]any{
			"access_key_id": "AK", "secret_access_key": "SK",
		}},
	}
}

// fakeVendor serves the three catalogue sources plus the credit endpoints.
func fakeVendor(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case codearts.AgentListPath:
			fmt.Fprint(w, `{"agents":[{"agent_id":"a-1","agent_name":"CodeAgent","show_in_ide":true,"alias":{"alias_zh_cn":"智能体"}}]}`)
		case codearts.AgentDetailPath:
			fmt.Fprint(w, `{"gpts":{"models":[{"model_name":"GLM-5.2","model_parameters":{"context_window":131072}}]}}`)
		case codearts.BuiltinPath:
			fmt.Fprint(w, `{"builtinModels":[{"model_id":"Qwen3-VL-235B"}]}`)
		case codearts.BenefitClaimPath:
			fmt.Fprint(w, `{"error_code":"0000"}`)
		case codearts.BenefitConfigPath:
			fmt.Fprint(w, `{"error_code":"0000","result":{"models":[{"model_id":"deepseek-v4.1-flash-1017"}]}}`)
		case codearts.DeliveryPath:
			fmt.Fprint(w, `{"code":0,"data":{"items":[{"campaignId":1,"title":"每日签到","status":"ELIGIBLE","claimable":true,"benefitUnit":"CREDIT","benefitAmount":1000}]}}`)
		case codearts.ClaimPath, codearts.ConfirmPath:
			fmt.Fprint(w, `{"code":0,"data":{}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDiscoverStoresTheCatalogue(t *testing.T) {
	srv := fakeVendor(t, nil)
	p := pool.New([]*accounts.Record{syncAccount("hw-1")})
	var saved *accounts.Record
	svc := &Service{Pool: p, Persist: func(r *accounts.Record) error { saved = r; return nil },
		Base: srv.URL, BenefitBase: srv.URL, ClaimBenefit: true}

	dis, err := svc.Discover(context.Background(), p.Get("hw-1"))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(dis.Models) != 3 {
		t.Fatalf("models = %+v", dis.Models)
	}
	if saved == nil {
		t.Fatal("the catalogue was not persisted")
	}
	rec := p.Get("hw-1").Record()
	models := codearts.ModelsOf(rec)
	if len(models) != 3 {
		t.Fatalf("stored models = %v", models)
	}
	if !codearts.IsBenefitModel(rec, "deepseek-v4.1-flash-1017") {
		t.Fatalf("benefit flag not stored: %v", codearts.Extra(rec)["benefit_models"])
	}
	if at, _ := codearts.Extra(rec)["models_at"].(float64); at == 0 {
		t.Fatal("models_at not recorded")
	}
	// The original record's map must not be mutated through the shared header.
	if _, ok := codearts.Extra(saved)["models_error"]; ok {
		t.Fatalf("unexpected models_error: %v", codearts.Extra(saved)["models_error"])
	}
}

// A failed discovery keeps the previous catalogue: an outage must not empty
// /v1/models.
func TestDiscoverFailureKeepsPreviousCatalogue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error_code":"denied"}`)
	}))
	defer srv.Close()

	rec := syncAccount("hw-1")
	rec.Extra["codearts"].(map[string]any)["models"] = []any{"GLM-5.2"}
	p := pool.New([]*accounts.Record{rec})
	svc := &Service{Pool: p, Base: srv.URL, BenefitBase: srv.URL}
	if _, err := svc.Discover(context.Background(), p.Get("hw-1")); err == nil {
		t.Fatal("expected an error when every source fails")
	}
	if models := codearts.ModelsOf(p.Get("hw-1").Record()); len(models) != 1 || models[0] != "GLM-5.2" {
		t.Fatalf("previous catalogue was lost: %v", models)
	}
}

func TestCheckinRecordsAndGatesTheDay(t *testing.T) {
	srv := fakeVendor(t, nil)
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	p := pool.New([]*accounts.Record{syncAccount("hw-1")})
	svc := &Service{Pool: p, Base: srv.URL, Now: func() time.Time { return now }}
	acc := p.Get("hw-1")

	if !svc.CanCheckin(acc) {
		t.Fatal("a fresh account needs its check-in")
	}
	ok, msg, err := svc.Checkin(context.Background(), acc)
	if err != nil || !ok {
		t.Fatalf("Checkin = (%v, %q, %v)", ok, msg, err)
	}
	st := acc.Record().LastCheckin.(map[string]any)
	if st["ok"] != true || st["credits"] != float64(1000) || st["channel"] != realm.CodeArts {
		t.Fatalf("LastCheckin = %#v", st)
	}
	if svc.CanCheckin(acc) {
		t.Fatal("a finished check-in must not run twice on the same day")
	}
	// Next day it is due again.
	svc.Now = func() time.Time { return now.Add(24 * time.Hour) }
	if !svc.CanCheckin(acc) {
		t.Fatal("the next day must be due again")
	}
}

func TestCanCheckinIgnoresOtherChannels(t *testing.T) {
	p := pool.New([]*accounts.Record{{UID: "cn-1", Realm: realm.CN, AccessToken: "t"}})
	svc := &Service{Pool: p}
	if svc.CanCheckin(p.Get("cn-1")) {
		t.Fatal("the WorkBuddy channel has its own scheduler path")
	}
	if svc.CanCheckin(nil) {
		t.Fatal("nil account must not be due")
	}
}

func TestScannerSkipsFreshAccounts(t *testing.T) {
	var hits atomic.Int32
	srv := fakeVendor(t, &hits)
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	fresh := syncAccount("hw-fresh")
	fresh.Extra["codearts"].(map[string]any)["models_at"] = float64(now.Add(-time.Minute).Unix())
	stale := syncAccount("hw-stale")
	stale.Extra["codearts"].(map[string]any)["models_at"] = float64(now.Add(-2 * time.Hour).Unix())
	other := &accounts.Record{UID: "cn-1", Realm: realm.CN, AccessToken: "t"}

	p := pool.New([]*accounts.Record{fresh, stale, other})
	svc := &Service{Pool: p, Base: srv.URL, BenefitBase: srv.URL, Now: func() time.Time { return now }}
	sc := &Scanner{Service: svc, Interval: 30 * time.Minute}
	if n := sc.ScanOnce(context.Background()); n != 1 {
		t.Fatalf("scanned %d accounts, want only the stale one", n)
	}
	if hits.Load() == 0 {
		t.Fatal("the stale account was not discovered")
	}
	if at, _ := codearts.Extra(p.Get("hw-stale").Record())["models_at"].(float64); at != float64(now.Unix()) {
		t.Fatalf("models_at = %v, want %d", at, now.Unix())
	}
	// After a pass it is fresh and the next scan skips it.
	before := hits.Load()
	if n := sc.ScanOnce(context.Background()); n != 0 {
		t.Fatalf("second scan found %d due accounts", n)
	}
	if hits.Load() != before {
		t.Fatal("a fresh account was scanned again")
	}
}

// A second pass on the same day must not downgrade a real claim to "无活动".
func TestCheckinKeepsTheDaysRealResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == codearts.DeliveryPath {
			fmt.Fprint(w, `{"code":0,"data":{"items":[]}}`)
			return
		}
		fmt.Fprint(w, `{"code":0,"data":{}}`)
	}))
	defer srv.Close()

	now := time.Date(2026, 11, 17, 21, 0, 0, 0, time.Local)
	rec := syncAccount("hw-1")
	rec.LastCheckin = map[string]any{
		"at": float64(now.Add(-2 * time.Minute).Unix()), "ok": true,
		"credits": float64(4000), "message": "新用户注册送 4000 积分: 已补确认", "channel": realm.CodeArts,
	}
	p := pool.New([]*accounts.Record{rec})
	svc := &Service{Pool: p, Base: srv.URL, Now: func() time.Time { return now }}
	if _, _, err := svc.Checkin(context.Background(), p.Get("hw-1")); err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	st := p.Get("hw-1").Record().LastCheckin.(map[string]any)
	if st["credits"] != float64(4000) || !strings.Contains(st["message"].(string), "4000") {
		t.Fatalf("the earlier result was downgraded: %#v", st)
	}
}
