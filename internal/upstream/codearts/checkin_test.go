package codearts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCreditProtocolPreservesCampaignIDTypeAndSigns(t *testing.T) {
	calls := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "SDK-HMAC-SHA256 Access=AK, ") || r.Header.Get("X-Sdk-Date") == "" {
			t.Error("missing credit request signature")
		}
		if r.URL.Path == DeliveryPath {
			if r.Method != http.MethodGet || r.URL.Query().Get("channel") != CreditChannel {
				t.Errorf("delivery request = %s %s", r.Method, r.URL)
			}
			fmt.Fprint(w, `{"code":0,"data":{"items":[{"campaignId":34,"type":"USER_LOGIN","benefitUnit":"CREDIT"},{"campaignId":"new-daily","type":"USER_LOGIN","benefitUnit":"CREDIT"}]}}`)
			return
		}
		var fields map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
			t.Error(err)
		}
		calls <- r.URL.Path + ":" + string(fields["campaignId"])
		if r.Method != http.MethodPost {
			t.Errorf("write method = %s", r.Method)
		}
		if r.URL.Path == ClaimPath && (string(fields["idempotentKey"]) != `"daily-key"` || string(fields["channel"]) != `"IDE"`) {
			t.Errorf("claim fields = %s", fields)
		}
		fmt.Fprintf(w, `{"code":0,"data":{"campaignId":%s}}`, fields["campaignId"])
	}))
	defer srv.Close()
	ctx := context.Background()
	cred := Credential{AccessKeyID: "AK", SecretAccessKey: "SK", SecurityToken: "STS"}
	items, err := FetchCampaigns(ctx, srv.Client(), srv.URL, cred)
	if err != nil || len(items) != 2 {
		t.Fatalf("items = %v, err = %v", items, err)
	}
	if items[0].Key() != "34" || items[1].Key() != "new-daily" {
		t.Fatalf("unexpected keys = %s, %s", items[0].Key(), items[1].Key())
	}
	for _, c := range items {
		if !c.IsDailyCredit() {
			t.Fatalf("campaign = %+v", c)
		}
		if err := ClaimCredit(ctx, srv.Client(), srv.URL, cred, c, "daily-key"); err != nil {
			t.Fatal(err)
		}
		if err := ConfirmCredit(ctx, srv.Client(), srv.URL, cred, c); err != nil {
			t.Fatal(err)
		}
	}
	close(calls)
	for _, want := range []string{ClaimPath + ":34", ConfirmPath + ":34", ClaimPath + `:"new-daily"`, ConfirmPath + `:"new-daily"`} {
		if got := <-calls; got != want {
			t.Fatalf("request = %q, want %q", got, want)
		}
	}
}

func TestFetchCampaignsRejectsBusinessError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":4004,"message":"benefit not found"}`)
	}))
	defer srv.Close()
	_, err := FetchCampaigns(context.Background(), srv.Client(), srv.URL, Credential{})
	if err == nil || !strings.Contains(err.Error(), "4004") {
		t.Fatalf("err = %v", err)
	}
}

func TestCreditRetryTimeUsesRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)
	err := &CreditHTTPError{Status: http.StatusTooManyRequests, RetryAfter: "120"}
	if got := CreditRetryTime(err, now); !got.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("retry time = %v", got)
	}
}
