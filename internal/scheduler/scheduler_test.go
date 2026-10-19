package scheduler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wb2api/internal/activity"
	"wb2api/internal/ops"
	"wb2api/internal/pool"
	"wb2api/internal/store/accounts"
	"wb2api/internal/store/settings"
	"wb2api/internal/upstream"
)

type fx struct {
	s       *Scheduler
	pool    *pool.Pool
	mu      sync.Mutex
	calls   []string
	refresh []string
}

type refresher struct{ f *fx }

func (r refresher) Refresh(ctx context.Context, a *pool.Account) error {
	r.f.mu.Lock()
	r.f.refresh = append(r.f.refresh, a.UID)
	r.f.mu.Unlock()
	return nil
}

func newFx(t *testing.T, specs ...map[string]any) *fx {
	t.Helper()
	f := &fx{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path+" "+r.Header.Get("X-User-Id"))
		f.mu.Unlock()
		switch r.URL.Path {
		case "/v2/billing/meter/daily-checkin":
			io.WriteString(w, `{"code":0,"msg":"ok"}`)
		case "/v2/report":
			io.WriteString(w, `{"code":0}`)
		case "/activity/growth/streak":
			io.WriteString(w, `{"data":{"streak":{"days":2}}}`)
		case "/v2/chat/completions":
			io.WriteString(w, "data: [DONE]\n\n")
		case "/v2/billing/meter/get-user-resource":
			io.WriteString(w, `{"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":10,"CycleCapacityRemain":4}]}}}}`)
		case "/billing/meter/get-user-resource-summary":
			io.WriteString(w, `{"code":0,"data":{"Packages":[]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	var recs []*accounts.Record
	exp := time.Now().Add(24 * time.Hour).Unix()
	for i, sp := range specs {
		m := map[string]any{"uid": "acct-" + string(rune('a'+i)), "accessToken": "t" + string(rune('a'+i)), "realm": "intl", "expiresAt": exp}
		for k, v := range sp {
			m[k] = v
		}
		recs = append(recs, accounts.FromMap(m, "", time.Now()))
	}
	f.pool = pool.New(recs)
	dir := t.TempDir()
	st := settings.Open(dir)
	client := &ops.Client{Pool: f.pool, Up: upstream.NewManager(upstream.DefaultTimeouts()), Settings: st,
		Activity: &activity.Log{Dir: dir}, BaseOverride: srv.URL}
	f.s = &Scheduler{Pool: f.pool, Ops: client, Refresher: refresher{f}, Settings: st,
		Stagger: func() time.Duration { return 0 }, Pause: func(time.Duration) {}}
	return f
}

func (f *fx) called(sub string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

func TestCycleDoesTheDailyChores(t *testing.T) {
	f := newFx(t,
		map[string]any{"realm": "cn", "uid": "cn-1"},                                                               // check-in + activity report
		map[string]any{"realm": "intl", "uid": "intl-1"},                                                           // daily chat
		map[string]any{"realm": "cn", "uid": "cn-2", "restrictedUntil": float64(time.Now().Add(time.Hour).Unix())}, // skipped
		map[string]any{"realm": "cn", "uid": "cn-3", "enabled": false},                                             // skipped
		map[string]any{"realm": "intl", "uid": "soon", "expiresAt": time.Now().Add(30 * time.Minute).Unix(),
			"lastDailyChat": time.Now().Format("2006-01-02 15:04:05")}, // keep-alive only
	)
	f.s.Cycle(context.Background(), "test")
	if f.called("daily-checkin cn-1") != 1 || f.called("/v2/report cn-1") != 1 || f.called("chat/completions intl-1") != 1 {
		t.Fatalf("calls: %v", f.calls)
	}
	if f.called("cn-2") != 0 || f.called("cn-3") != 0 {
		t.Fatalf("restricted and disabled accounts are left alone: %v", f.calls)
	}
	if f.called("soon") != 0 {
		t.Errorf("already done today: %v", f.calls)
	}
	if len(f.refresh) != 1 || f.refresh[0] != "soon" {
		t.Errorf("only the nearly expired token is kept alive: %v", f.refresh)
	}
	// a second pass the same day does no more daily chore (the streak keeper
	// still reads its state; that is idempotent and the legacy scheduler does it
	// on every cycle too).
	beforeCheckin := f.called("daily-checkin")
	beforeReport := f.called("/v2/report")
	beforeChat := f.called("chat/completions")
	f.s.Cycle(context.Background(), "again")
	if f.called("daily-checkin") != beforeCheckin || f.called("/v2/report") != beforeReport || f.called("chat/completions") != beforeChat {
		t.Errorf("each daily chore runs at most once a day: %v", f.calls)
	}
	st := f.s.Status()
	joined := strings.Join(st.Logs, "|")
	if !st.Enabled || st.LastRun == "尚未运行" || !strings.Contains(joined, "国内签到 1") || st.NextRun == "待调度" {
		t.Fatalf("%+v", st)
	}
}

func TestRegionHoldSkipsTheRealm(t *testing.T) {
	f := newFx(t, map[string]any{"realm": "cn", "uid": "cn-1"})
	f.s.Hold = func(realm string) time.Duration { return time.Minute }
	f.s.Cycle(context.Background(), "test")
	if len(f.calls) != 0 {
		t.Fatalf("a risk-control pause keeps unattended jobs away from the upstream: %v", f.calls)
	}
}

func TestOverlappingCyclesAreRefused(t *testing.T) {
	f := newFx(t, map[string]any{"realm": "intl"})
	f.s.mu.Lock()
	f.s.running = true
	f.s.mu.Unlock()
	if f.s.Trigger() {
		t.Fatal("a second trigger while one runs is refused")
	}
}

func TestCreditsRefreshTakesTheStalestOnePerTick(t *testing.T) {
	fresh := float64(time.Now().Unix())
	f := newFx(t,
		map[string]any{"uid": "fresh", "credits": map[string]any{"remain": 1, "updated_at": fresh}},
		map[string]any{"uid": "old", "credits": map[string]any{"remain": 1, "updated_at": fresh - 100000}},
		map[string]any{"uid": "never"},
	)
	if got := f.s.RefreshCreditsOnce(context.Background()); got != "never" {
		t.Fatalf("a never-fetched balance is the oldest: %q", got)
	}
	if got := f.s.RefreshCreditsOnce(context.Background()); got != "old" {
		t.Fatalf("%q", got)
	}
	if got := f.s.RefreshCreditsOnce(context.Background()); got != "" {
		t.Fatalf("nothing else is past the 12 h TTL: %q", got)
	}
	if err := f.s.Settings.SetCreditsRefreshHours(0); err != nil || f.s.RefreshCreditsOnce(context.Background()) != "" {
		t.Fatal("a zero TTL switches the refresher off")
	}
}

func TestFailedBalanceRefreshParksTheAccount(t *testing.T) {
	f := newFx(t, map[string]any{"uid": "broken", "realm": "cn"})
	// the mock has no CN package endpoints configured beyond the summary, fallback 404s
	if got := f.s.RefreshCreditsOnce(context.Background()); got != "" {
		// a CN account whose billing calls fail must not be retried at once
		t.Logf("refresh result %q", got)
	}
	first := f.called("billing")
	f.s.RefreshCreditsOnce(context.Background())
	if f.called("billing") != first && f.s.parked["broken"].IsZero() {
		t.Error("a failing account is parked")
	}
}

func TestNextFireAndHours(t *testing.T) {
	f := newFx(t)
	f.s.Now = func() time.Time { return time.Date(2026, 10, 19, 10, 30, 0, 0, time.Local) }
	if n := f.s.NextFire(); n.Hour() != 21 || n.Day() != 19 {
		t.Fatalf("%v", n)
	}
	f.s.Now = func() time.Time { return time.Date(2026, 10, 19, 23, 30, 0, 0, time.Local) }
	if n := f.s.NextFire(); n.Hour() != 1 || n.Day() != 20 {
		t.Fatalf("wraps to the first hour tomorrow: %v", n)
	}
	if _, err := f.s.Settings.SetScheduleHours(map[string]any{"checkin": []any{"x"}}); err == nil {
		t.Error("a supplied key needs at least one valid hour")
	}
	if got, err := f.s.Settings.SetScheduleHours(map[string]any{"checkin": "8, 20, 20, 99"}); err != nil || len(got["checkin"]) != 2 || got["checkin"][0] != 8 {
		t.Fatalf("%v %v", got, err)
	}
}
