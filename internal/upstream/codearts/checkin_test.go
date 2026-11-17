package codearts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// creditServer emulates the delivery / claim / confirm endpoints.
type creditServer struct {
	mu      sync.Mutex
	written []string
	srv     *httptest.Server
	sawAuth []string
	sawDate []string
}

func newCreditServer(t *testing.T, campaigns string, claimFails bool) *creditServer {
	t.Helper()
	cs := &creditServer{}
	cs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.mu.Lock()
		cs.sawAuth = append(cs.sawAuth, r.Header.Get("Authorization"))
		cs.sawDate = append(cs.sawDate, r.Header.Get("X-Sdk-Date"))
		cs.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == DeliveryPath:
			if got := r.URL.Query().Get("channel"); got != CreditChannel {
				t.Errorf("delivery channel = %q", got)
			}
			fmt.Fprintf(w, `{"code":0,"data":{"items":%s}}`, campaigns)
		case r.URL.Path == ClaimPath:
			cs.mu.Lock()
			cs.written = append(cs.written, "claim")
			cs.mu.Unlock()
			if claimFails {
				fmt.Fprint(w, `{"code":4004,"message":"claim rejected"}`)
				return
			}
			fmt.Fprint(w, `{"code":0,"data":{}}`)
		case r.URL.Path == ConfirmPath:
			cs.mu.Lock()
			cs.written = append(cs.written, "confirm")
			cs.mu.Unlock()
			fmt.Fprint(w, `{"code":0,"data":{}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(cs.srv.Close)
	return cs
}

func (c *creditServer) writes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.written...)
}

func creditCred() Credential {
	return Credential{AccessKeyID: "AK", SecretAccessKey: "SK", SecurityToken: "STS"}
}

func TestCheckinClaimsEligibleCampaign(t *testing.T) {
	cs := newCreditServer(t, `[
		{"campaignId":1,"title":"每日签到","status":"ELIGIBLE","claimable":true,"benefitUnit":"CREDIT","benefitAmount":1000},
		{"campaignId":2,"title":"别的活动","status":"ELIGIBLE","claimable":true,"benefitAmount":50}
	]`, false)
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	res, err := Checkin(context.Background(), cs.srv.Client(), cs.srv.URL, creditCred(), now)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK || res.Credits != 1000 {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Campaigns) != 1 || res.Campaigns[0].CampaignID != 1 {
		t.Fatalf("campaigns = %+v (only ids 1 and 4 are claimed)", res.Campaigns)
	}
	if got := strings.Join(cs.writes(), ","); got != "claim,confirm" {
		t.Fatalf("writes = %s", got)
	}
	if len(cs.sawAuth) != 3 {
		t.Fatalf("expected three signed calls, got %d", len(cs.sawAuth))
	}
	for i, a := range cs.sawAuth {
		if !strings.HasPrefix(a, "SDK-HMAC-SHA256 Access=AK, ") {
			t.Fatalf("call %d is unsigned: %q", i, a)
		}
		if cs.sawDate[i] == "" {
			t.Fatalf("call %d has no signing date", i)
		}
	}
}

// A CLAIMED campaign is credited but unconfirmed: confirm only, never claim
// twice.
func TestCheckinConfirmsWithoutReclaiming(t *testing.T) {
	cs := newCreditServer(t, `[
		{"campaignId":1,"title":"每日签到","status":"CLAIMED","benefitUnit":"CREDIT","pendingTotalAmount":1000}
	]`, false)
	res, err := Checkin(context.Background(), cs.srv.Client(), cs.srv.URL, creditCred(), time.Now())
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("result = %+v", res)
	}
	if got := strings.Join(cs.writes(), ","); got != "confirm" {
		t.Fatalf("writes = %s, want only confirm", got)
	}
}

func TestCheckinConfirmedCampaignWritesNothing(t *testing.T) {
	cs := newCreditServer(t, `[
		{"campaignId":1,"title":"每日签到","status":"CONFIRMED","benefitUnit":"CREDIT"}
	]`, false)
	res, err := Checkin(context.Background(), cs.srv.Client(), cs.srv.URL, creditCred(), time.Now())
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if got := cs.writes(); len(got) != 0 {
		t.Fatalf("writes = %v, want none", got)
	}
	if !strings.Contains(res.Message, "已领取") {
		t.Fatalf("message = %q", res.Message)
	}
}

func TestCheckinReportsUpstreamClaimFailure(t *testing.T) {
	cs := newCreditServer(t, `[
		{"campaignId":1,"title":"每日签到","status":"ELIGIBLE","claimable":true,"benefitAmount":1000}
	]`, true)
	res, err := Checkin(context.Background(), cs.srv.Client(), cs.srv.URL, creditCred(), time.Now())
	if err != nil {
		t.Fatalf("Checkin returned a transport error: %v", err)
	}
	if res.OK {
		t.Fatal("a rejected claim must not be reported as success")
	}
	if !strings.Contains(res.Message, "4004") {
		t.Fatalf("message = %q", res.Message)
	}
}

func TestCheckinSkipsEndedCampaign(t *testing.T) {
	cs := newCreditServer(t, `[
		{"campaignId":1,"title":"每日签到","status":"ELIGIBLE","claimable":true,"benefitAmount":1000,
		 "extra":{"startTime":"2026-01-01T00:00:00Z","endTime":"2026-02-01T00:00:00Z"}}
	]`, false)
	res, err := Checkin(context.Background(), cs.srv.Client(), cs.srv.URL, creditCred(), time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if len(cs.writes()) != 0 {
		t.Fatal("an ended campaign must not be claimed")
	}
	if !strings.Contains(res.Message, "已结束") {
		t.Fatalf("message = %q", res.Message)
	}
}

// The envelope code is the upstream's business result: a non-zero code is an
// error even on HTTP 200.
func TestFetchCampaignsRejectsBusinessError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":4004,"message":"benefit not found"}`)
	}))
	defer srv.Close()
	_, err := FetchCampaigns(context.Background(), srv.Client(), srv.URL, creditCred())
	if err == nil || !strings.Contains(err.Error(), "4004") {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchCampaignsParsesItems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":{"items":[{"campaignId":4,"title":"新用户","status":"ELIGIBLE","claimable":true,"benefitAmount":4000}]}}`)
	}))
	defer srv.Close()
	items, err := FetchCampaigns(context.Background(), srv.Client(), srv.URL, creditCred())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(items)
	if !strings.Contains(string(b), `"campaignId":4`) {
		t.Fatalf("items = %s", b)
	}
}
