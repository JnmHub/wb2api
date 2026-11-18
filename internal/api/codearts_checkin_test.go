package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wb2api/internal/codeartsync"
	"wb2api/internal/ops"
	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream/codearts"
)

func codeartsAccountRec(uid string) *accounts.Record {
	return &accounts.Record{
		UID: uid, Realm: realm.CodeArts, AccessToken: "STS",
		RefreshToken: "refresh",
		Extra: map[string]any{"codearts": map[string]any{
			"access_key_id": "AK", "secret_access_key": "SK",
		}},
	}
}

// vendor serves the credit endpoints and prints nothing else. Claimed
// campaigns come back CONFIRMED, because the gateway re-reads the list.
func vendor(t *testing.T) *httptest.Server {
	t.Helper()
	confirmed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case codearts.DeliveryPath:
			status := "ELIGIBLE"
			if confirmed {
				status = "CONFIRMED"
			}
			fmt.Fprintf(w, `{"code":0,"data":{"items":[{"campaignId":1,"title":"每日签到","status":"%s","claimable":true,"benefitUnit":"CREDIT","benefitAmount":1000}]}}`, status)
		case codearts.ClaimPath, codearts.ConfirmPath:
			confirmed = true
			fmt.Fprint(w, `{"code":0,"data":{}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func serviceFor(p *pool.Pool, srv *httptest.Server, persist func(*accounts.Record) error, now time.Time) *codeartsync.Service {
	return &codeartsync.Service{
		Pool: p, Persist: persist, Base: srv.URL, BenefitBase: srv.URL,
		Now: func() time.Time { return now },
	}
}

// The manual check-in must hit the channel's credit endpoints (not WorkBuddy's)
// and record the outcome on the account so the panel shows it.
func TestCheckinCodeArtsRecordsStatus(t *testing.T) {
	srv := vendor(t)
	p := pool.New([]*accounts.Record{codeartsAccountRec("hw-1")})
	var saved *accounts.Record
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	a := &Accounts{Pool: p, CodeArts: serviceFor(p, srv, func(r *accounts.Record) error { saved = r; return nil }, now)}

	if err := a.checkinCodeArts(context.Background(), p.Get("hw-1")); err != nil {
		t.Fatalf("checkinCodeArts: %v", err)
	}
	if saved == nil {
		t.Fatal("the result was not persisted")
	}
	st, ok := saved.LastCheckin.(map[string]any)
	if !ok {
		t.Fatalf("LastCheckin = %#v", saved.LastCheckin)
	}
	if st["ok"] != true || st["channel"] != realm.CodeArts || st["credits"] != float64(1000) {
		t.Fatalf("status = %#v", st)
	}
}

// A business failure is reported as an error and still recorded.
func TestCheckinCodeArtsFailureIsRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == codearts.DeliveryPath {
			fmt.Fprint(w, `{"code":0,"data":{"items":[{"campaignId":1,"title":"每日签到","status":"ELIGIBLE","claimable":true,"benefitAmount":1000}]}}`)
			return
		}
		fmt.Fprint(w, `{"code":4004,"message":"claim rejected"}`)
	}))
	defer srv.Close()

	p := pool.New([]*accounts.Record{codeartsAccountRec("hw-1")})
	var saved *accounts.Record
	a := &Accounts{Pool: p, CodeArts: serviceFor(p, srv, func(r *accounts.Record) error { saved = r; return nil }, time.Now())}
	if err := a.checkinCodeArts(context.Background(), p.Get("hw-1")); err == nil {
		t.Fatal("a rejected claim must surface as an error")
	}
	st := saved.LastCheckin.(map[string]any)
	if st["ok"] != false || !strings.Contains(fmt.Sprint(st["message"]), "4004") {
		t.Fatalf("status = %#v", st)
	}
}

func TestCheckinCodeArtsWithoutService(t *testing.T) {
	p := pool.New([]*accounts.Record{codeartsAccountRec("hw-1")})
	a := &Accounts{Pool: p}
	if err := a.checkinCodeArts(context.Background(), p.Get("hw-1")); err == nil {
		t.Fatal("a disabled channel must report an error")
	}
}

// The batch "签到" action must dispatch per account by channel.
func TestBatchCheckinDispatchesToCodeArts(t *testing.T) {
	srv := vendor(t)
	p := pool.New([]*accounts.Record{codeartsAccountRec("hw-1")})
	var saved *accounts.Record
	a := &Accounts{
		Pool: p, Ops: &ops.Client{}, Gate: NewOpsGate(1, 0),
		CodeArts: serviceFor(p, srv, func(r *accounts.Record) error { saved = r; return nil }, time.Now()),
	}
	fn, err := a.upstreamAction(batchReq{Action: "checkin"})
	if err != nil {
		t.Fatalf("upstreamAction: %v", err)
	}
	if err := fn(context.Background(), "hw-1"); err != nil {
		t.Fatalf("job: %v", err)
	}
	if saved == nil {
		t.Fatal("the batch check-in did not record the result")
	}
}

// The single-account action endpoint must dispatch to the channel too: the
// drawer's 签到 button posts here, not through the batch job.
func TestOperationsActionCheckinCodeArts(t *testing.T) {
	srv := vendor(t)
	p := pool.New([]*accounts.Record{codeartsAccountRec("hw-1")})
	var saved *accounts.Record
	o := &Operations{
		Pool:     p,
		Persist:  func(r *accounts.Record) error { saved = r; return nil },
		CodeArts: serviceFor(p, srv, func(r *accounts.Record) error { saved = r; return nil }, time.Now()),
	}
	mux := http.NewServeMux()
	o.Register(mux, func(h http.Handler) http.Handler { return h })

	req := httptest.NewRequest(http.MethodPost, "/api/v2/accounts/hw-1/actions/checkin", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, w.Body.String())
	}
	if out["ok"] != true {
		t.Fatalf("action result = %#v", out)
	}
	if saved == nil {
		t.Fatal("the account was not updated")
	}
}

// The batch filter must let the channel through for check-in (it is no longer
// CN-only) while still rejecting an INTL-only WorkBuddy account.
func TestFilterBatchRealmCoversBothCheckinChannels(t *testing.T) {
	enterprise := codeartsAccountRec("cn-ent")
	enterprise.Realm = realm.CN
	enterprise.EnterpriseID = "ent-1"

	recs := []*accounts.Record{
		codeartsAccountRec("hw-1"),
		{UID: "cn-1", Realm: realm.CN, AccessToken: "t"},
		enterprise,
		{UID: "intl-1", Realm: realm.Intl, AccessToken: "t"},
	}
	a := &Accounts{Pool: pool.New(recs)}
	got, msg := a.filterBatchRealm("checkin", []string{"hw-1", "cn-1", "cn-ent", "intl-1"})
	if msg != "" {
		t.Fatalf("unexpected message %q", msg)
	}
	if strings.Join(got, ",") != "hw-1,cn-1" {
		t.Fatalf("kept = %v, want the CodeArts and the domestic account", got)
	}
	if _, msg := a.filterBatchRealm("checkin", []string{"intl-1"}); msg == "" {
		t.Fatal("an intl-only selection must explain why nothing runs")
	}
	got, _ = a.filterBatchRealm("daily_chat", []string{"hw-1", "intl-1"})
	if strings.Join(got, ",") != "intl-1" {
		t.Fatalf("daily chat kept = %v", got)
	}
}

// "刷新积分" on a CodeArts account reads the subscription statistics and stores
// the aggregated balance on the record.
func TestCodeArtsCreditsActionReadsStatistics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != codearts.StatisticsPath {
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "SDK-HMAC-SHA256 Access=AK") {
			t.Errorf("statistics call not signed: %q", r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{"end_date":"2026-12-01","package":{"spec_code":"trial","package_name_en":"Trial"},
			"metrics":[
			  {"name":"usageTotalPackageCredit","show":true,"package_credit_amount":10345.14,"package_credit_remain":5172.57},
			  {"name":"usageBasicPackageCredit","show":true,"package_credit_amount":345.14,"package_credit_remain":172.57},
			  {"name":"usageBonusPackageCredit","show":true,"package_credit_amount":10000,"package_credit_remain":5000}
			]}`)
	}))
	defer srv.Close()

	p := pool.New([]*accounts.Record{codeartsAccountRec("hw-1")})
	var saved *accounts.Record
	o := &Operations{
		Pool:    p,
		Persist: func(r *accounts.Record) error { saved = r; return nil },
		CodeArts: &codeartsync.Service{Pool: p, Base: srv.URL,
			Persist: func(r *accounts.Record) error { saved = r; return nil }},
	}
	mux := http.NewServeMux()
	o.Register(mux, func(h http.Handler) http.Handler { return h })
	req := httptest.NewRequest(http.MethodPost, "/api/v2/accounts/hw-1/actions/credits", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["ok"] != true || out["remain"] != 5172.57 {
		t.Fatalf("action result = %#v", out)
	}
	if saved == nil || saved.Credits == nil {
		t.Fatal("the balance was not stored on the record")
	}
	if saved.Credits["remain"] != 5172.57 || saved.Credits["plan"] != "Trial" {
		t.Fatalf("stored credits = %#v", saved.Credits)
	}
}
