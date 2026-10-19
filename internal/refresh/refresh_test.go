package refresh

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream"
)

func jwt(exp int64) string {
	seg := func(o any) string {
		b, _ := json.Marshal(o)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return seg(map[string]any{"alg": "none"}) + "." + seg(map[string]any{"sub": "u", "exp": exp}) + ".sig"
}

func TestPyTitleGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/title.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs []struct{ In, Out string }
	if err := json.Unmarshal(raw, &vs); err != nil || len(vs) == 0 {
		t.Fatal(err)
	}
	for _, v := range vs {
		if got := pyTitle(v.In); got != v.Out {
			t.Errorf("pyTitle(%q)=%q want %q", v.In, got, v.Out)
		}
	}
}

type fakeUp struct {
	srv     *httptest.Server
	calls   atomic.Int32
	status  atomic.Int32 // 0 = 200
	body    atomic.Value // string
	gotHdr  atomic.Value // http.Header (raw names are lost; check values)
	gotBody atomic.Value
	delay   atomic.Int64
}

func newFakeUp(t *testing.T) *fakeUp {
	f := &fakeUp{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		f.gotHdr.Store(r.Header.Clone())
		b := make([]byte, 16)
		n, _ := r.Body.Read(b)
		f.gotBody.Store(string(b[:n]))
		if d := f.delay.Load(); d > 0 {
			time.Sleep(time.Duration(d))
		}
		if r.URL.Path != RefreshPath {
			w.WriteHeader(404)
			return
		}
		if s := int(f.status.Load()); s != 0 {
			w.WriteHeader(s)
			return
		}
		fmt.Fprint(w, f.body.Load().(string))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

type rig struct {
	r     *Refresher
	p     *pool.Pool
	up    *fakeUp
	dir   string
	clock atomic.Int64
	saved []*accounts.Record
	mu    sync.Mutex
}

func newRig(t *testing.T, expiresIn time.Duration, n int) *rig {
	t.Helper()
	g := &rig{up: newFakeUp(t), dir: t.TempDir()}
	g.clock.Store(time.Now().UnixNano())
	var recs []*accounts.Record
	for i := 0; i < n; i++ {
		exp := time.Now().Add(expiresIn).Unix()
		recs = append(recs, accounts.FromMap(map[string]any{
			"uid": fmt.Sprintf("acct-%d", i), "accessToken": jwt(exp), "refreshToken": "ref-old",
			"realm": "intl", "expiresAt": exp,
		}, "", time.Now()))
	}
	g.p = pool.New(recs)
	g.r = &Refresher{
		Pool: g.p, Up: upstream.NewManager(upstream.DefaultTimeouts()),
		BaseURL: func(*pool.Account) string { return g.up.srv.URL },
		Now:     func() time.Time { return time.Unix(0, g.clock.Load()) },
		Persist: func(rec *accounts.Record) error {
			g.mu.Lock()
			g.saved = append(g.saved, rec)
			g.mu.Unlock()
			_, err := accounts.Save(g.dir, rec)
			return err
		},
	}
	return g
}

func (g *rig) setBody(access string, refresh string, nested bool) {
	inner := fmt.Sprintf(`{"accessToken":%q,"refreshToken":%q}`, access, refresh)
	if nested {
		g.up.body.Store(fmt.Sprintf(`{"code":0,"data":{"data":%s}}`, inner))
	} else {
		g.up.body.Store(fmt.Sprintf(`{"code":0,"data":%s}`, inner))
	}
}

func TestRefreshSuccessUpdatesPoolDiskAndReadiness(t *testing.T) {
	g := newRig(t, 60*time.Second, 1) // inside the 120 s "not ready" window
	acc := g.p.All()[0]
	if acc.Ready("m", time.Now()) {
		t.Fatal("a token about to expire must not be handed out")
	}
	newExp := time.Now().Add(2 * time.Hour).Unix()
	g.setBody(jwt(newExp), "ref-new", true)
	if err := g.r.Refresh(context.Background(), acc); err != nil {
		t.Fatal(err)
	}
	if !acc.Ready("m", time.Now()) {
		t.Fatal("refreshed account should be ready")
	}
	rec := acc.Record()
	if rec.RefreshToken != "ref-new" || rec.ExpiresAt != newExp {
		t.Fatalf("record not updated: %+v", rec)
	}
	back, errs := accounts.Load(g.dir, time.Now())
	if len(errs) != 0 || len(back) != 1 || back[0].RefreshToken != "ref-new" || back[0].AccessToken != rec.AccessToken {
		t.Fatalf("not persisted: %v %v", back, errs)
	}
	h := g.up.gotHdr.Load().(http.Header)
	if h.Get("X-Refresh-Token") != "ref-old" || h.Get("X-User-Id") != "acct-0" || h.Get("X-Auth-Refresh-Source") != "plugin" {
		t.Fatalf("bad request headers: %v", h)
	}
	if g.up.gotBody.Load() != "{}" {
		t.Fatalf("body %q", g.up.gotBody.Load())
	}
}

func TestRefreshAcceptsFlatResponseShape(t *testing.T) {
	g := newRig(t, 30*time.Second, 1)
	g.setBody(jwt(time.Now().Add(time.Hour).Unix()), "", false)
	if err := g.r.Refresh(context.Background(), g.p.All()[0]); err != nil {
		t.Fatal(err)
	}
	if g.p.All()[0].Record().RefreshToken != "ref-old" {
		t.Fatal("an absent refreshToken must keep the old one")
	}
}

func TestRefresh401MarksNeedsLoginAndStopsRetrying(t *testing.T) {
	g := newRig(t, 30*time.Second, 1)
	g.up.status.Store(401)
	acc := g.p.All()[0]
	err := g.r.Refresh(context.Background(), acc)
	if !errors.Is(err, ErrNeedsLogin) {
		t.Fatalf("want ErrNeedsLogin, got %v", err)
	}
	if acc.Ready("m", time.Now()) {
		t.Fatal("account needing re-login must not be ready")
	}
	before := g.up.calls.Load()
	g.r.ScanOnce(context.Background())
	if g.up.calls.Load() != before {
		t.Fatal("scanner must not hammer an account that needs a re-login")
	}
}

func TestRefreshNoTokenInBodyNeedsLogin(t *testing.T) {
	g := newRig(t, 30*time.Second, 1)
	g.up.body.Store(`{"code":5,"msg":"nope","data":{}}`)
	if err := g.r.Refresh(context.Background(), g.p.All()[0]); !errors.Is(err, ErrNeedsLogin) || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("got %v", err)
	}
}

func TestTransientFailureBacksOffExponentially(t *testing.T) {
	g := newRig(t, 30*time.Second, 1)
	g.up.status.Store(500)
	acc := g.p.All()[0]
	ctx := context.Background()

	_ = g.r.Refresh(ctx, acc)
	st := g.r.StatusOf(acc.UID)
	if st.Failures != 1 || !st.NextTry.After(g.r.now().Add(25*time.Second)) {
		t.Fatalf("first backoff: %+v", st)
	}
	// Inside the backoff window the scanner leaves it alone.
	calls := g.up.calls.Load()
	if g.r.ScanOnce(ctx); g.up.calls.Load() != calls {
		t.Fatal("scan retried inside the backoff window")
	}
	// Once the window passed it tries again and the delay doubles.
	g.clock.Add(int64(31 * time.Second))
	if g.r.ScanOnce(ctx) != 1 {
		t.Fatal("scan should retry after the backoff")
	}
	st2 := g.r.StatusOf(acc.UID)
	if st2.Failures != 2 || st2.NextTry.Sub(g.r.now()) < 55*time.Second {
		t.Fatalf("second backoff should be ~60s: %+v", st2)
	}
	// A transient failure never marks the credential as dead.
	g.setBody(jwt(time.Now().Add(time.Hour).Unix()), "r2", true)
	g.up.status.Store(0)
	g.clock.Add(int64(2 * time.Minute))
	if g.r.ScanOnce(ctx) != 1 || g.r.StatusOf(acc.UID).Failures != 0 {
		t.Fatalf("recovery should clear the failure count: %+v", g.r.StatusOf(acc.UID))
	}
}

func TestConcurrentRefreshesShareOneUpstreamCall(t *testing.T) {
	g := newRig(t, 30*time.Second, 1)
	g.up.delay.Store(int64(150 * time.Millisecond))
	g.setBody(jwt(time.Now().Add(time.Hour).Unix()), "ref-new", true)
	acc := g.p.All()[0]
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := g.r.Refresh(context.Background(), acc); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := g.up.calls.Load(); n != 1 {
		t.Fatalf("20 concurrent refreshes caused %d upstream calls (the refresh token is single-use)", n)
	}
}

func TestScanRefreshesOnlyDueAccountsAndSpreadsLoad(t *testing.T) {
	g := newRig(t, 6*time.Hour, 3) // far from expiry
	g.setBody(jwt(time.Now().Add(8*time.Hour).Unix()), "r", true)
	if n := g.r.ScanOnce(context.Background()); n != 0 {
		t.Fatalf("nothing is due yet, scanned %d", n)
	}
	// Move the clock to just inside every account's window (lead + jitter <= 22.5 min).
	g.clock.Add(int64(6*time.Hour - 10*time.Minute))
	if n := g.r.ScanOnce(context.Background()); n != 3 {
		t.Fatalf("all three accounts are due, scanned %d", n)
	}
	// Different uids get different jitter so a big pool does not refresh in one burst.
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		seen[g.r.jitter(fmt.Sprintf("uid-%d", i))] = true
	}
	if len(seen) < 25 {
		t.Fatalf("jitter is not spreading: %d distinct values", len(seen))
	}
}

func TestPickNeverBlocksOnRefresh(t *testing.T) {
	// The whole point of the package: a slow refresh endpoint must not affect
	// request-path account selection.
	g := newRig(t, 30*time.Second, 3)
	g.up.delay.Store(int64(500 * time.Millisecond))
	g.setBody(jwt(time.Now().Add(time.Hour).Unix()), "r", true)
	g.p.All()[1].SetExpiresAt(time.Now().Add(time.Hour).Unix()) // one healthy account
	go g.r.ScanOnce(context.Background())
	time.Sleep(50 * time.Millisecond) // refreshes are now in flight
	start := time.Now()
	for i := 0; i < 1000; i++ {
		if a := g.p.Pick("intl", "m", nil); a == nil || a.UID != "acct-1" {
			t.Fatalf("expected the healthy account, got %v", a)
		}
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("selection took %v while refreshes were in flight", d)
	}
}
