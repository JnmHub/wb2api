package codeartsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/activity"
	"wb2api/internal/pool"
	"wb2api/internal/refresh"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream/codearts"
)

func TestDailyIgnoresNewUserAndUsesBeijingDay(t *testing.T) {
	now := time.Date(2026, 11, 20, 15, 56, 0, 0, time.UTC)
	var read atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != codearts.DeliveryPath {
			t.Errorf("unexpected write %s", r.URL.Path)
		}
		read.Add(1)
		fmt.Fprint(w, `{"code":0,"data":{"items":[
			{"campaignId":4,"type":"NEW_USER_REGISTER","benefitUnit":"CREDIT","status":"ELIGIBLE","claimable":true},
			{"campaignId":9,"type":"USER_LOGIN","benefitUnit":"VIP","status":"ELIGIBLE","claimable":true},
			{"campaignId":34,"type":"USER_LOGIN","benefitUnit":"CREDIT","status":"ELIGIBLE","claimable":true,"extra":{"endTime":"2026-09-20T00:00:00Z"}}
		]}}`)
	}))
	defer srv.Close()
	rec := syncAccount("hw-1")
	rec.LastCheckin = map[string]any{"ok": true, "credits": 4000, "at": now.Unix(), "channel": "codearts"}
	p := pool.New([]*accounts.Record{rec})
	svc := &Service{Pool: p, Base: srv.URL, Now: func() time.Time { return now }}
	acc := p.Get(rec.UID)

	res, err := svc.RunDaily(context.Background(), acc, "scheduler")
	if err != nil || res.OK || res.Credits != 0 || res.State != DailyUnavailable || res.Day != "2026-11-20" {
		t.Fatalf("day one = %+v, err = %v", res, err)
	}
	if svc.CanCheckin(acc) {
		t.Fatal("availability polling ran again before its deadline")
	}
	now = now.Add(5 * time.Minute)
	res, err = svc.RunDaily(context.Background(), acc, "scheduler")
	if err != nil || res.Day != "2026-11-21" || read.Load() != 2 {
		t.Fatalf("new Beijing day = %+v, err = %v, reads = %d", res, err, read.Load())
	}
}

func TestDailyRestartsBetweenClaimAndVerification(t *testing.T) {
	now := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	ctx := context.Background()
	dir := t.TempDir()
	history := &activity.Log{Dir: dir, Now: func() time.Time { return now }}
	var reads, claims, confirms atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case codearts.DeliveryPath:
			reads.Add(1)
			state := ""
			if claims.Load() > 0 {
				state = "CLAIMED"
			}
			if confirms.Load() > 1 {
				state = "CONSUMED"
			}
			fmt.Fprintf(w, `{"code":0,"data":{"items":[{"campaignId":"ca-daily","type":"USER_LOGIN","benefitUnit":"CREDIT","status":"%s","claimable":%t,"pendingTotalAmount":100}]}}`, state, state == "")
		case codearts.ClaimPath:
			claims.Add(1)
			fmt.Fprint(w, `{"code":0,"data":{"campaignId":"ca-daily"}}`)
		case codearts.ConfirmPath:
			if confirms.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			fmt.Fprint(w, `{"code":0,"data":{}}`)
		case codearts.StatisticsPath:
			fmt.Fprint(w, `{"metrics":[{"name":"usageBonusPackageCredit","show":true,"package_credit_amount":100,"package_credit_remain":100}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	makeService := func(p *pool.Pool) *Service {
		return &Service{Pool: p, Activity: history, Base: srv.URL, Now: func() time.Time { return now }, Persist: func(rec *accounts.Record) error {
			latest, err := accounts.SaveReconciled(dir, rec, now)
			if latest != nil {
				p.ApplyChanges([]*accounts.Record{latest}, nil)
			}
			return err
		}}
	}
	rec := syncAccount("hw-1")
	p := pool.New([]*accounts.Record{rec})
	res, err := makeService(p).RunDaily(ctx, p.Get(rec.UID), "scheduler")
	if err != nil || res.State != DailyPending || res.Credits != 0 {
		t.Fatalf("first pass = %+v, err = %v", res, err)
	}
	actual, bad := accounts.Load(dir, now)
	if len(actual) != 1 || len(bad) != 0 {
		t.Fatalf("persisted accounts = %v, rejected = %v", len(actual), bad)
	}
	p = pool.New(actual)
	svc := makeService(p)
	acc := p.Get(rec.UID)
	if res, err := svc.RunDaily(ctx, acc, "manual"); err != nil || res.State != DailyPending || reads.Load() != 1 {
		t.Fatalf("manual click ignored persisted gap: result = %+v, reads = %d, err = %v", res, reads.Load(), err)
	}
	now = now.Add(minClaimGap)
	res, err = svc.RunDaily(ctx, acc, "makeup")
	if err != nil || res.State != DailyClaimed || res.Credits != 100 || reads.Load() != 3 || claims.Load() != 1 || confirms.Load() != 2 {
		t.Fatalf("restarted outcome = %+v, reads = %d, claims = %d, confirms = %d, err = %v", res, reads.Load(), claims.Load(), confirms.Load(), err)
	}
	if got := history.Query(activity.Filter{Task: activity.TaskCodeArts, Result: DailyClaimed}); len(got.Rows) != 1 || got.Rows[0].Credits != 100 || got.Rows[0].Trigger != "makeup" {
		t.Fatalf("activity = %+v", got)
	}
}

func TestDailyPersistsWriteBudgetAndReusesKey(t *testing.T) {
	now := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	keys := make(chan string, 8)
	var confirms atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case codearts.DeliveryPath:
			fmt.Fprint(w, `{"code":0,"data":{"items":[{"campaignId":34,"type":"USER_LOGIN","benefitUnit":"CREDIT","status":"ELIGIBLE","claimable":true}]}}`)
		case codearts.ClaimPath:
			var body struct {
				Key string `json:"idempotentKey"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			keys <- body.Key
			w.WriteHeader(http.StatusServiceUnavailable)
		case codearts.ConfirmPath:
			confirms.Add(1)
		}
	}))
	defer srv.Close()
	rec := syncAccount("hw-1")
	p := pool.New([]*accounts.Record{rec})
	svc := &Service{Pool: p, Base: srv.URL, Now: func() time.Time { return now }}
	for attempt := 1; attempt <= maxClaimAttempts; attempt++ {
		res, err := svc.RunDaily(context.Background(), p.Get(rec.UID), "manual")
		if err != nil || (attempt < maxClaimAttempts && res.State != DailyPending) || (attempt == maxClaimAttempts && res.State != DailyExhausted) {
			t.Fatalf("attempt %d = %+v, err = %v", attempt, res, err)
		}
		now = now.Add(minClaimGap)
		// A new service still reads the budget from the saved record.
		svc = &Service{Pool: p, Base: srv.URL, Now: func() time.Time { return now }}
	}
	if len(keys) != maxClaimAttempts-1 || confirms.Load() != 0 {
		t.Fatalf("claim writes = %d, confirm writes = %d", len(keys), confirms.Load())
	}
	close(keys)
	first := <-keys
	if first == "" {
		t.Fatal("missing idempotency key")
	}
	for key := range keys {
		if key != first {
			t.Fatalf("key %q changed from %q", key, first)
		}
	}
}

func TestDailyRefreshBackoffDoesNotConsumeWriteBudget(t *testing.T) {
	now := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	rec := syncAccount("hw-1")
	rec.ExpiresAt = now.Unix()
	p := pool.New([]*accounts.Record{rec})
	ctx := context.Background()
	retry := now.Add(25 * time.Minute)
	svc := &Service{Pool: p, Now: func() time.Time { return now }, Refresh: func(context.Context, *pool.Account) error {
		if calls.Add(1) == 1 {
			return &refresh.RetryError{At: retry, Cause: errors.New("HTTP 429")}
		}
		return refresh.ErrNeedsLogin
	}}
	acc := p.Get(rec.UID)
	res, err := svc.RunDaily(ctx, acc, "scheduler")
	if err != nil || res.State != DailyWaiting || res.NextRetry != retry.Unix() || len(dailyOf(acc.Record(), now).Progress) != 0 {
		t.Fatalf("refresh throttle = %+v, err = %v", res, err)
	}
	if _, err := svc.RunDaily(ctx, acc, "manual"); err != nil || calls.Load() != 1 {
		t.Fatal("manual interruption of the shared credential backoff")
	}
	now = retry
	res, err = svc.RunDaily(ctx, acc, "makeup")
	if err != nil || res.State != DailyNeedsLogin || len(dailyOf(acc.Record(), now).Progress) != 0 {
		t.Fatalf("terminal auth = %+v, err = %v", res, err)
	}
}

func TestDailyRefusesUnpersistedWrites(t *testing.T) {
	var writes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writes.Add(1)
		}
		fmt.Fprint(w, `{"code":0,"data":{"items":[{"campaignId":34,"type":"USER_LOGIN","benefitUnit":"CREDIT","status":"ELIGIBLE","claimable":true}]}}`)
	}))
	defer srv.Close()
	p := pool.New([]*accounts.Record{syncAccount("hw-1")})
	svc := &Service{Pool: p, Base: srv.URL, Persist: func(*accounts.Record) error { return errors.New("disk full") }}
	if _, err := svc.RunDaily(context.Background(), p.Get("hw-1"), "scheduler"); err == nil || writes.Load() != 0 {
		t.Fatalf("writes = %d, err = %v", writes.Load(), err)
	}
}

func TestConfirmedDailyWithEmptyBalanceRetriesOnlyStatistics(t *testing.T) {
	now := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	var stats atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != codearts.StatisticsPath {
			t.Errorf("completed check-in resent %s", r.URL.Path)
		}
		if stats.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"metrics":[{"name":"usageBonusPackageCredit","show":true,"package_credit_amount":100,"package_credit_remain":100}]}`)
	}))
	defer srv.Close()
	rec := syncAccount("hw-1")
	rec.Credits = map[string]any{"remain": 0}
	rec.LastCheckin = map[string]any{"at": now.Unix(), "day": claimDay(now), "channel": "codearts",
		"state": DailyAlready, "ok": true, "message": "官方活动已确认领取"}
	p := pool.New([]*accounts.Record{rec})
	acc := p.Get(rec.UID)
	acc.NoteBalanceCooled("no credits", now)
	svc := &Service{Pool: p, Base: srv.URL, Now: func() time.Time { return now }}
	ctx := context.Background()
	if res, err := svc.RunDaily(ctx, acc, "makeup"); err != nil || !res.OK || stats.Load() != 1 {
		t.Fatalf("balance failure changed confirmed result: %+v, err = %v, stats = %d", res, err, stats.Load())
	}
	if svc.CanCheckin(acc) {
		t.Fatal("balance failure immediately scheduled another attempt")
	}
	now = now.Add(balanceRetryGap)
	if res, err := svc.RunDaily(ctx, acc, "makeup"); err != nil || !res.OK || stats.Load() != 2 {
		t.Fatalf("balance retry = %+v, err = %v, stats = %d", res, err, stats.Load())
	}
	if remain, ok := acc.CreditRemain(); !ok || remain != 100 || !acc.Ready("model", now) {
		t.Fatalf("confirmed balance = %g, known = %v, schedulable = %v", remain, ok, acc.Ready("model", now))
	}
}

func TestBalanceDoesNotResendRejectedAccessAfterRefreshRefusal(t *testing.T) {
	now := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error_code":"APIG.0602","error_msg":"the security token has expired"}`)
	}))
	defer srv.Close()
	p := pool.New([]*accounts.Record{syncAccount("hw-1"),
		syncAccount("hw-terminal").WithCodeArtsAuth(accounts.CodeArtsAuth{Login: true, Reason: "invalid_grant"})})
	svc := &Service{Pool: p, Base: srv.URL, Now: func() time.Time { return now },
		RefreshObserved: func(context.Context, *pool.Account, *accounts.Record) error {
			return &refresh.RetryError{At: now.Add(time.Minute), Cause: errors.New("HTTP 429")}
		}}
	ctx := context.Background()
	acc := p.Get("hw-1")
	if _, err := svc.Balance(ctx, acc); err == nil || hits.Load() != 1 || !acc.CodeArtsAccessRejected() {
		t.Fatalf("balance rejection: err = %v, sends = %d", err, hits.Load())
	}
	if _, err := svc.Discover(ctx, acc); err == nil || hits.Load() != 1 {
		t.Fatalf("discovery reused rejected access: err = %v, sends = %d", err, hits.Load())
	}
	if (&Scanner{Service: svc}).ModelRefreshDue(p.Get("hw-terminal")) {
		t.Fatal("scanner selected an account with terminal refresh rejection")
	}
}
