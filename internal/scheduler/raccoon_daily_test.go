package scheduler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/ops"
	"wb2api/internal/pool"
	"wb2api/internal/raccoonsync"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
	"wb2api/internal/store/settings"
	"wb2api/internal/upstream/raccoon"
)

func TestRaccoonScheduledCheckinQueuesAndHonorsSwitch(t *testing.T) {
	now := time.Date(2026, 11, 20, 13, 0, 0, 0, time.UTC)
	started, release := make(chan struct{}), make(chan struct{})
	var unlock sync.Once
	var checkins atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case raccoon.BillsPath:
			fmt.Fprint(w, `{"code":0,"data":{"items":[{"biz_type":"daily_grant","points":100,"created_at":"2026-11-20 10:00:00"}]}}`)
		case raccoon.SettingInfoPersonalPath:
			if checkins.Add(1) == 1 {
				close(started)
				<-release
			}
			fmt.Fprint(w, `{"code":0,"data":{}}`)
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()
	defer unlock.Do(func() { close(release) })
	p := pool.New([]*accounts.Record{newRaccoonFixture("rc-one", now), newRaccoonFixture("rc-two", now)})
	st := settings.Open(t.TempDir())
	s := &Scheduler{Pool: p, Settings: st, Now: func() time.Time { return now }, Stagger: func() time.Duration { return 0 },
		Raccoon: &raccoonsync.Service{Pool: p, Now: func() time.Time { return now }, Client: &raccoon.Client{HTTP: server.Client(), Origin: server.URL}}}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	queued := make(chan bool, 1)
	go func() { queued <- s.runRaccoon(ctx, false) }()
	select {
	case ok := <-queued:
		if !ok {
			t.Fatal("expected eligible Raccoon accounts to be queued")
		}
	case <-ctx.Done():
		t.Fatal("Raccoon blocked the scheduler while waiting for upstream")
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("queued account did not reach the upstream")
	}
	if s.TriggerRaccoon() {
		t.Fatal("concurrent manual pass should not queue")
	}
	if err := st.SetSchedulerEnabled(false); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	done := s.raccoon.done
	s.mu.Unlock()
	unlock.Do(func() { close(release) })
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("daily job did not finish")
	}
	if checkins.Load() != 1 || s.raccoonDailyStatus().States["done"] != 1 {
		t.Fatal("the scheduler ignored the new setting or lost today's successful result")
	}
	if err := st.SetSchedulerEnabled(true); err != nil {
		t.Fatal(err)
	}
	completed := p.All()[0]
	if !checkedInToday(completed.Record(), now) {
		completed = p.All()[1]
	}
	uids := []string{completed.UID}
	if err := st.SetRaccoonDaily(settings.RaccoonDailyPatch{UIDs: &uids}); err != nil {
		t.Fatal(err)
	}
	if s.runRaccoon(ctx, false) || s.raccoonDailyStatus().States["due"] != 0 {
		t.Fatal("already checked-in account caused another automatic pass")
	}
}

func TestScheduledRaccoonBalanceUsesRaccoonUpstream(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case raccoon.BalancePath:
			calls.Add(1)
			fmt.Fprint(w, `{"code":0,"data":{"available_points":355}}`)
		case raccoon.EntitlementPath:
			fmt.Fprint(w, `{"code":0,"data":{}}`)
		default:
			t.Errorf("wrong upstream request %s %s", r.Method, r.URL.Path)
			http.Error(w, "wrong provider", http.StatusUnauthorized)
		}
	}))
	defer server.Close()
	now := time.Now()
	p := pool.New([]*accounts.Record{newRaccoonFixture("rc-balance", now)})
	st := settings.Open(t.TempDir())
	if err := st.Save(settings.Doc{"credits_refresh_hours": 4}); err != nil {
		t.Fatal(err)
	}
	s := &Scheduler{Pool: p, Settings: st, Ops: &ops.Client{BaseOverride: server.URL},
		Raccoon: &raccoonsync.Service{Pool: p, Client: &raccoon.Client{HTTP: server.Client(), Origin: server.URL}}}
	if uid := s.RefreshCreditsOnce(context.Background()); uid != "rc-balance" || calls.Load() != 1 {
		t.Fatalf("balance worker did not use the Raccoon service: uid=%q calls=%d", uid, calls.Load())
	}
	if uid := s.RefreshCreditsOnce(context.Background()); uid != "" || calls.Load() != 1 {
		t.Fatalf("fresh Raccoon balance consumed another slot: uid=%q calls=%d", uid, calls.Load())
	}
}

func newRaccoonFixture(uid string, now time.Time) *accounts.Record {
	return accounts.FromMap(map[string]any{"uid": uid, "realm": realm.Raccoon, "accessToken": "synthetic-token", "expiresAt": now.Add(time.Hour).Unix()}, "", now)
}
