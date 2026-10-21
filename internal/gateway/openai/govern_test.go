package openai

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/catalog"
	"wb2api/internal/pool"
	"wb2api/internal/quota"
	"wb2api/internal/store/settings"
)

func conv(n int) string {
	return fmt.Sprintf(`{"model":"glm-5.3","messages":[{"role":"user","content":"conversation %d"}]}`, n)
}

func tokOf(r rawReq) string { return r.header("Authorization") }

func guardState(dailyToken bool) pool.GuardState { return pool.GuardState{DailyToken: dailyToken} }

func (f *fixture) setSettings(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, "settings.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(settings.CheckInterval + 100*time.Millisecond)
}

func TestAccountLevel429ParksTheCredentialNotJustTheModel(t *testing.T) {
	var n atomic.Int32
	up := newRawUpstream(t, func(i int, r rawReq, c net.Conn) {
		if n.Add(1) == 1 {
			status(c, 429, `{"msg":"this account has hit its limit"}`)
			return
		}
		sseOK(c)
	})
	f := newFixture(t, up, accSpec{}, accSpec{})
	code, _, _ := f.post(t, conv(1), nil)
	if code != 200 {
		t.Fatalf("second account must serve: %d", code)
	}
	var parked, free int
	for _, a := range f.pool.All() {
		if !a.Ready("glm-5.3", time.Now()) {
			parked++
			if w := a.ThrottleWait("other-model", time.Now()); w < 9*time.Minute {
				t.Errorf("an account-level limit parks every model (~10 min), got %v", w)
			}
		} else {
			free++
		}
	}
	if parked != 1 || free != 1 {
		t.Fatalf("parked=%d free=%d", parked, free)
	}
}

func TestUnscoped429OnlyParksThatModelAndGrows(t *testing.T) {
	up := newRawUpstream(t, func(i int, r rawReq, c net.Conn) {
		status(c, 429, `{"msg":"usage exceeds frequency limit"}`)
	})
	f := newFixture(t, up, accSpec{})
	code, body, hdr := f.post(t, conv(1), nil)
	if code != 429 || hdr.Get("Retry-After") == "" {
		t.Fatalf("exhausted pool must answer 429 with Retry-After: %d %s %v", code, body, hdr)
	}
	var env struct {
		Error struct {
			Type       string `json:"type"`
			Code       int    `json:"code"`
			RetryAfter int    `json:"retry_after"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil || env.Error.Type != "rate_limit_error" || env.Error.Code != 429 || env.Error.RetryAfter < 1 {
		t.Fatalf("envelope %s", body)
	}
	a := f.pool.All()[0]
	if !a.Ready("other", time.Now()) {
		t.Error("an unscoped 429 must leave the account usable for other models")
	}
	if a.Ready("glm-5.3", time.Now()) {
		t.Error("...but park the model")
	}
	if a.UnscopedStreak() != 1 {
		t.Errorf("streak %d", a.UnscopedStreak())
	}
}

func TestQuotaCap6004IsPersistedForFreeModels(t *testing.T) {
	reset := time.Now().Add(3 * time.Hour).In(time.FixedZone("x", 8*3600))
	body := fmt.Sprintf(`{"code":6004,"msg":"your usage will reset at %s UTC+8"}`, reset.Format("2006-01-02 15:04:05"))
	up := newRawUpstream(t, func(i int, r rawReq, c net.Conn) { status(c, 429, body) })
	f := newFixture(t, up, accSpec{})
	code, _, hdr := f.post(t, `{"model":"hy3","messages":[{"role":"user","content":"x"}]}`, nil)
	if code != 429 {
		t.Fatalf("%d", code)
	}
	if ra := hdr.Get("Retry-After"); ra == "" || ra == "60" {
		t.Errorf("Retry-After should follow the upstream clock, got %q", ra)
	}
	f.pmu.Lock()
	defer f.pmu.Unlock()
	if len(f.persisted) == 0 || f.persisted[len(f.persisted)-1].ModelCaps["hy3"].Reset == 0 {
		t.Fatalf("the 6004 window must be written to the account file: %v", f.persisted)
	}
}

func TestModel429RegistersAWindowProbe(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).In(time.FixedZone("x", 8*3600))
	body := fmt.Sprintf(`{"code":6004,"msg":"your usage will reset at %s UTC+8"}`, reset.Format("2006-01-02 15:04:05"))
	up := newRawUpstream(t, func(i int, r rawReq, c net.Conn) { status(c, 429, body) })
	f := newFixture(t, up, accSpec{})
	w := &quota.Windows{Dir: t.TempDir()}
	f.h.Windows = w
	if code, _, _ := f.post(t, `{"model":"hy3","messages":[{"role":"user","content":"x"}]}`, nil); code != 429 {
		t.Fatalf("status %d", code)
	}
	if !w.Has("acct-0", "hy3") {
		t.Fatal("a model-scoped 429 with a reset clock must register a window")
	}
	if got := w.Snapshot(); len(got) != 1 || got[0].Attempts != 0 {
		t.Fatalf("window row %+v", got)
	}
}

func TestFreeModelFallsBackToPaidSiblingWhenEveryAccountIsCapped(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).In(time.FixedZone("x", 8*3600))
	freeBody := fmt.Sprintf(`{"code":6004,"msg":"your usage will reset at %s UTC+8"}`, reset.Format("2006-01-02 15:04:05"))
	var paid atomic.Int32
	up := newRawUpstream(t, func(n int, r rawReq, c net.Conn) {
		if strings.Contains(string(r.body), "deepseek-v4.1-flash-sg") {
			paid.Add(1)
			sseOK(c)
			return
		}
		status(c, 429, freeBody)
	})
	f := newFixture(t, up, accSpec{}, accSpec{})
	f.setSettings(t, `{"free_model_credit_fallback":true}`)
	code, body, _ := f.post(t, `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"x"}]}`, nil)
	if code != 200 {
		t.Fatalf("paid fallback should serve: %d %s", code, body)
	}
	if paid.Load() == 0 {
		t.Fatal("the paid sibling was never requested")
	}
	reqs := up.requests()
	if len(reqs) < 3 {
		t.Fatalf("expected two free-model attempts then the paid one, got %d", len(reqs))
	}
	if !strings.Contains(string(reqs[0].body), `"deepseek-v4.1-flash"`) {
		t.Fatalf("first attempt should target the free model: %s", reqs[0].body)
	}
	last := reqs[len(reqs)-1]
	if !strings.Contains(string(last.body), `"deepseek-v4.1-flash-sg"`) {
		t.Fatalf("last attempt should target the paid sibling: %s", last.body)
	}
}

func TestPaymentRequiredParksUntilTheDailyReset(t *testing.T) {
	var n atomic.Int32
	up := newRawUpstream(t, func(i int, r rawReq, c net.Conn) {
		if n.Add(1) == 1 {
			status(c, 402, `{"msg":"insufficient credits"}`)
			return
		}
		sseOK(c)
	})
	f := newFixture(t, up, accSpec{}, accSpec{})
	if code, _, _ := f.post(t, conv(1), nil); code != 200 {
		t.Fatalf("%d", code)
	}
	parked := 0
	for _, a := range f.pool.All() {
		if w := a.ThrottleWait("glm-5.3", time.Now()); w > 0 {
			parked++
			if w > 25*time.Hour {
				t.Errorf("park should end at the next 04:00, got %v", w)
			}
			if r := a.UnavailableReason("glm-5.3", time.Now()); !strings.Contains(r, "余额保护") {
				t.Errorf("reason %q", r)
			}
		}
	}
	if parked != 1 {
		t.Fatalf("parked %d", parked)
	}
}

func TestRepeated5xxTripsTheBreakerAcrossRequests(t *testing.T) {
	up := newRawUpstream(t, func(i int, r rawReq, c net.Conn) { status(c, 500, `{"msg":"boom"}`) })
	f := newFixture(t, up, accSpec{})
	for i := 0; i < 3; i++ {
		code, _, _ := f.post(t, conv(i), nil)
		if code != 500 {
			t.Fatalf("request %d: the last upstream status is passed through, got %d", i, code)
		}
	}
	a := f.pool.All()[0]
	if a.Ready("glm-5.3", time.Now()) || !strings.Contains(a.UnavailableReason("glm-5.3", time.Now()), "熔断") {
		t.Fatalf("three consecutive hard failures open the breaker: %q", a.UnavailableReason("glm-5.3", time.Now()))
	}
	code, body, _ := f.post(t, conv(9), nil)
	if code != 503 || !strings.Contains(body, "no usable account") || !strings.Contains(body, "熔断") {
		t.Fatalf("the 503 must say why: %d %s", code, body)
	}
}

func TestOneRequestChargesAnAccountOnlyOnce(t *testing.T) {
	up := newRawUpstream(t, func(i int, r rawReq, c net.Conn) { status(c, 500, `x`) })
	f := newFixture(t, up, accSpec{})
	f.post(t, conv(1), nil) // several attempts inside one request
	_, _, fails, _ := f.pool.All()[0].Streaks()
	if fails != 1 {
		t.Fatalf("fails=%d: a retry round must not stack charges", fails)
	}
}

func TestEgressBackoffKeepsAccountsHealthy(t *testing.T) {
	up := newRawUpstream(t, func(i int, r rawReq, c net.Conn) { status(c, 502, `bad gateway`) })
	f := newFixture(t, up, accSpec{})
	f.setSettings(t, `{"egress_backoff_enabled":true}`)
	start := time.Now()
	code, _, _ := f.post(t, conv(1), nil)
	if code != 502 {
		t.Fatalf("%d", code)
	}
	a := f.pool.All()[0]
	_, _, fails, _ := a.Streaks()
	if fails != 0 || !a.Ready("glm-5.3", time.Now()) {
		t.Fatalf("path trouble must not be charged to the account (fails=%d)", fails)
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Errorf("the path should have been backed off for a moment: %v", time.Since(start))
	}
}

func TestUnauthorizedMarksTheCredential(t *testing.T) {
	var n atomic.Int32
	up := newRawUpstream(t, func(i int, r rawReq, c net.Conn) {
		if n.Add(1) == 1 {
			status(c, 401, `{"msg":"token expired"}`)
			return
		}
		sseOK(c)
	})
	f := newFixture(t, up, accSpec{}, accSpec{})
	if code, _, _ := f.post(t, conv(1), nil); code != 200 {
		t.Fatalf("%d", code)
	}
	bad := 0
	for _, a := range f.pool.All() {
		if a.TokenBad() {
			bad++
		}
	}
	if bad != 1 {
		t.Fatalf("a 401 that survives refresh marks the account needs-login, got %d", bad)
	}
}

func TestPrefixAffinityKeepsAConversationOnOneAccount(t *testing.T) {
	var seen []string
	up := newRawUpstream(t, func(i int, r rawReq, c net.Conn) { seen = append(seen, tokOf(r)); sseOK(c) })
	f := newFixture(t, up, accSpec{}, accSpec{}, accSpec{})
	for i := 0; i < 6; i++ {
		f.post(t, conv(1), nil)
	}
	distinct := map[string]bool{}
	for _, s := range seen {
		distinct[s] = true
	}
	if len(distinct) != 1 {
		t.Fatalf("one conversation must keep its account: %v", distinct)
	}
	seen = nil
	for i := 0; i < 12; i++ {
		f.post(t, conv(100+i), nil)
	}
	distinct = map[string]bool{}
	for _, s := range seen {
		distinct[s] = true
	}
	if len(distinct) < 2 {
		t.Fatalf("different conversations must spread over accounts: %v", distinct)
	}
}

func TestExplicitSessionKeyWinsAndFailureMovesTheConversation(t *testing.T) {
	var n atomic.Int32
	up := newRawUpstream(t, func(i int, r rawReq, c net.Conn) {
		if n.Add(1) == 2 { // the second request's account is rate limited
			status(c, 429, `{"msg":"usage exceeds frequency limit"}`)
			return
		}
		sseOK(c)
	})
	f := newFixture(t, up, accSpec{}, accSpec{})
	hdr := map[string]string{"X-Conversation-Id": "conv-A"}
	f.post(t, conv(1), hdr)
	f.post(t, conv(2), hdr) // same session key: same account first, 429, then the other one
	reqs := up.requests()
	if len(reqs) != 3 || tokOf(reqs[0]) != tokOf(reqs[1]) || tokOf(reqs[2]) == tokOf(reqs[1]) {
		t.Fatalf("session key must pin, and a failed account must be demoted: %v", func() []string {
			var o []string
			for _, r := range reqs {
				o = append(o, tokOf(r))
			}
			return o
		}())
	}
}

func TestManualWeightsPreferHeavyAccounts(t *testing.T) {
	var counts = map[string]int{}
	up := newRawUpstream(t, func(i int, r rawReq, c net.Conn) { counts[tokOf(r)]++; sseOK(c) })
	f := newFixture(t, up, accSpec{"weight": 4.0}, accSpec{}, accSpec{"weight": 2.0})
	for i := 0; i < 30; i++ {
		f.post(t, conv(1000+i), nil)
	}
	if counts["Bearer tok-1"] != 0 || counts["Bearer tok-0"] < 15 || counts["Bearer tok-2"] < 5 {
		t.Fatalf("weighted accounts take the traffic in proportion (4:2), the weight-1 account only as a fallback: %v", counts)
	}
}

func TestDailyTokenGuardParksThePool(t *testing.T) {
	up := newRawUpstream(t, func(i int, r rawReq, c net.Conn) { sseOK(c) })
	f := newFixture(t, up, accSpec{})
	f.pool.All()[0].SetGuards(guardState(true))
	code, body, hdr := f.post(t, conv(1), nil)
	if code != 503 && code != 429 {
		t.Fatalf("%d %s", code, body)
	}
	if !strings.Contains(body, "今日 Token 额度用尽") && !strings.Contains(body, "token limit") {
		t.Errorf("the reason must be named: %s (retry-after %q)", body, hdr.Get("Retry-After"))
	}
}

func TestPinnedRouteUsesExactlyThatAccount(t *testing.T) {
	var seen []string
	up := newRawUpstream(t, func(i int, r rawReq, c net.Conn) { seen = append(seen, tokOf(r)); sseOK(c) })
	f := newFixture(t, up, accSpec{"nickname": "alpha"}, accSpec{"nickname": "beta"})
	srv := httptest.NewServer(PinnedRoutes(f.pool, f.h))
	defer srv.Close()
	post := func(path string) (int, string) {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(conv(7)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for i := 0; i < 4; i++ {
		if code, body := post("/u/beta/v1/chat/completions"); code != 200 {
			t.Fatalf("%d %s", code, body)
		}
	}
	for _, s := range seen {
		if s != "Bearer tok-1" {
			t.Fatalf("pinned to beta but %s served", s)
		}
	}
	if code, _ := post("/u/acct-0/x"); code != 200 {
		t.Errorf("exact uid: %d", code)
	}
	if code, body := post("/u/nobody/v1/chat/completions"); code != 404 || !strings.Contains(body, "was not found") {
		t.Errorf("unknown ref: %d %s", code, body)
	}
	if code, body := post("/u/acct-/v1/chat/completions"); code != 409 || !strings.Contains(body, "ambiguous") {
		t.Errorf("a prefix matching two accounts is ambiguous: %d %s", code, body)
	}
	if code, _ := post("/u/beta"); code != 404 {
		t.Errorf("malformed pinned route: %d", code)
	}
	// a pinned account that cannot serve is an error, not a silent failover
	f.pool.All()[1].CoolDown(time.Now().Add(time.Hour))
	if code, body := post("/u/beta/v1/chat/completions"); code != 503 || !strings.Contains(body, "not usable") {
		t.Errorf("pinned but cooling: %d %s", code, body)
	}
}

func TestCrossRealmModelIsExplained(t *testing.T) {
	up := newRawUpstream(t, func(i int, r rawReq, c net.Conn) { sseOK(c) })
	f := newFixture(t, up, accSpec{})
	code, body, _ := f.post(t, `{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"x"}]}`, nil)
	if code != 400 || !strings.Contains(body, "只在国内版提供") {
		t.Fatalf("%d %s", code, body)
	}
	if len(up.requests()) != 0 {
		t.Error("a doomed request must not reach the upstream")
	}
}

func TestBackgroundModelIsRewrittenToAFreeOne(t *testing.T) {
	up := newRawUpstream(t, func(i int, r rawReq, c net.Conn) { sseOK(c) })
	f := newFixture(t, up, accSpec{})
	f.post(t, `{"model":"gpt-5.6-luna","messages":[{"role":"user","content":"x"}]}`, nil)
	if got := up.requests()[0].jsonBody(t).Get("model"); got != "deepseek-v4.1-flash" {
		t.Fatalf("upstream saw %v", got)
	}
}

func TestKeyConcurrencyIsNotCapped(t *testing.T) {
	release := make(chan struct{})
	var n atomic.Int32
	up := newRawUpstream(t, func(i int, r rawReq, c net.Conn) {
		// Hold the first request in flight; the second must not be rejected and
		// can answer immediately.
		if n.Add(1) == 1 {
			<-release
		}
		sseOK(c)
	})
	f := newFixture(t, up, accSpec{}, accSpec{})
	f.setSettings(t, `{"api_keys":[{"id":"k1","name":"unlimited","key":"sk-1","enabled":true,"concurrency_limit":1}]}`)
	hdr := map[string]string{"Authorization": "Bearer sk-1"}
	first := make(chan int, 1)
	go func() { code, _, _ := f.post(t, conv(1), hdr); first <- code }()
	time.Sleep(300 * time.Millisecond)
	// A stored limit of 1 must no longer reject the second request.
	code, body, _ := f.post(t, conv(2), hdr)
	if code != 200 {
		t.Fatalf("the gateway must not enforce concurrency limits: %d %s", code, body)
	}
	close(release)
	if c := <-first; c != 200 {
		t.Fatalf("the first request still completes: %d", c)
	}
}

func TestEffortIsRecordedOnTheUsageEvent(t *testing.T) {
	up := newRawUpstream(t, func(i int, r rawReq, c net.Conn) { sseOK(c) })
	f := newFixture(t, up, accSpec{})
	f.h.Catalog = &catalog.Service{}
	ch := make(chan UsageEvent, 1)
	f.h.OnUsage = func(ev UsageEvent) { ch <- ev }
	f.post(t, `{"model":"glm-5.3","reasoning_effort":"high","stream":true,"messages":[{"role":"user","content":"x"}]}`, nil)
	if ev := <-ch; ev.Effort != "high" {
		t.Fatalf("effort %q", ev.Effort)
	}
}

func TestModelScoped5xxDoesNotChargeAccount(t *testing.T) {
	up := newRawUpstream(t, func(i int, r rawReq, c net.Conn) {
		status(c, 500, `{"code":11134,"msg":"the model provider is temporarily unavailable"}`)
	})
	f := newFixture(t, up, accSpec{})
	for i := 0; i < 3; i++ {
		code, _, _ := f.post(t, conv(i), nil)
		if code != 500 {
			t.Fatalf("request %d: status %d", i, code)
		}
	}
	a := f.pool.All()[0]
	if !a.Ready("glm-5.3", time.Now()) {
		t.Fatalf("a model-scoped 500 must not park the account: %q", a.UnavailableReason("glm-5.3", time.Now()))
	}
	if _, _, fails, _ := a.Streaks(); fails != 0 {
		t.Fatalf("a model-scoped 500 must not charge the account: fails=%d", fails)
	}
}
