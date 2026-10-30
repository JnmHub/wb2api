package openai

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// A model that is exhausted on every account walks one step down the
// configured chain, and the client still sees the model it asked for.
func TestModelChainAdvancesWhenEveryAccountIsBlocked(t *testing.T) {
	const flash, bunny = "deepseek-v4.1-flash", "space-bunny"
	up := newRawUpstream(t, func(n int, r rawReq, c net.Conn) {
		if strings.Contains(string(r.body), bunny) {
			sseOK(c)
			return
		}
		status(c, 429, `{"error":{"message":"slow down"}}`)
	})
	f := newFixture(t, up, accSpec{}, accSpec{})
	if err := f.h.Settings.SetModelFallbacks(map[string][]string{flash: {bunny}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, a := range f.pool.All() {
		if _, ok := a.NoteModelCap(flash, now.Add(time.Hour), now); !ok {
			t.Fatalf("could not park %s for %s", a.Record().UID, flash)
		}
	}

	body := `{"model":"` + flash + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	code, out, _ := f.post(t, body, nil)
	if code != 200 {
		t.Fatalf("chain request failed: %d %s", code, out)
	}
	if !strings.Contains(out, `"model":"`+flash+`"`) {
		t.Fatalf("response must echo the requested model:\n%s", out)
	}
	if strings.Contains(out, bunny) {
		t.Fatalf("the fallback model leaked to the client:\n%s", out)
	}
	sawBunny := false
	for _, r := range up.requests() {
		if strings.Contains(string(r.body), bunny) {
			sawBunny = true
		}
	}
	if !sawBunny {
		t.Fatal("no upstream request used the fallback model")
	}
}

// With one healthy account left, the chain must not be touched: option B is
// staged switching, not a per-account swap.
func TestModelChainStaysPutWhileAnyAccountCanServe(t *testing.T) {
	const flash, bunny = "deepseek-v4.1-flash", "space-bunny"
	up := newRawUpstream(t, func(n int, r rawReq, c net.Conn) {
		if strings.Contains(string(r.body), bunny) {
			status(c, 500, `{"error":{"message":"wrong model"}}`)
			return
		}
		sseOK(c)
	})
	f := newFixture(t, up, accSpec{})
	if err := f.h.Settings.SetModelFallbacks(map[string][]string{flash: {bunny}}); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"` + flash + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	code, out, _ := f.post(t, body, nil)
	if code != 200 {
		t.Fatalf("status %d %s", code, out)
	}
	for _, r := range up.requests() {
		if strings.Contains(string(r.body), bunny) {
			t.Fatal("chain advanced although an account could serve the model")
		}
	}
}

// A chain step that is itself exhausted is skipped instead of burning an
// attempt loop on it.
func TestModelChainSkipsBlockedSteps(t *testing.T) {
	const flash, bunny, third = "deepseek-v4.1-flash", "space-bunny", "deepseek-v4.1-flash-sg"
	up := newRawUpstream(t, func(n int, r rawReq, c net.Conn) {
		if strings.Contains(string(r.body), third) {
			sseOK(c)
			return
		}
		status(c, 429, `{"error":{"message":"slow down"}}`)
	})
	f := newFixture(t, up, accSpec{})
	if err := f.h.Settings.SetModelFallbacks(map[string][]string{flash: {bunny, third}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, a := range f.pool.All() {
		a.NoteModelCap(flash, now.Add(time.Hour), now)
		a.NoteModelCap(bunny, now.Add(time.Hour), now)
	}
	body := `{"model":"` + flash + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	code, out, _ := f.post(t, body, nil)
	if code != 200 {
		t.Fatalf("status %d %s", code, out)
	}
	if !strings.Contains(out, `"model":"`+flash+`"`) {
		t.Fatalf("echo missing:\n%s", out)
	}
}

// fakeWindows is a ModelWindowStore with no Redis behind it.
type fakeWindows struct {
	blocked map[string]time.Time
	marks   map[string]time.Time
	cleared []string
	err     error
}

func (f *fakeWindows) Mark(_ context.Context, _, _, uid string, until time.Time) error {
	if f.err != nil {
		return f.err
	}
	if f.marks == nil {
		f.marks = map[string]time.Time{}
	}
	f.marks[uid] = until
	return nil
}

func (f *fakeWindows) Clear(_ context.Context, _, _, uid string) error {
	f.cleared = append(f.cleared, uid)
	return nil
}

func (f *fakeWindows) Blocked(_ context.Context, realmName, model string, uids []string) (map[string]time.Time, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]time.Time{}
	for _, uid := range uids {
		if until, ok := f.blocked[uid]; ok {
			out[uid] = until
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// A window learned by another replica is adopted locally and that account is
// skipped, without any local 429 having been seen.
func TestSharedModelWindowSkipsAccountLearnedElsewhere(t *testing.T) {
	const modelName = "glm-5.3"
	up := newRawUpstream(t, func(n int, r rawReq, c net.Conn) {
		if strings.Contains(strings.Join(r.head, "\n"), "Bearer tok-0") {
			t.Error("acct-0 was dispatched although another replica blocked it")
		}
		sseOK(c)
	})
	f := newFixture(t, up, accSpec{}, accSpec{})
	win := &fakeWindows{blocked: map[string]time.Time{"acct-0": time.Now().Add(30 * time.Minute)}}
	f.h.ModelWindows = win

	code, body, _ := f.post(t, `{"model":"`+modelName+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if code != 200 {
		t.Fatalf("status %d %s", code, body)
	}
	if until := f.pool.Get("acct-0").ModelCoolUntil(modelName, time.Now()); until.IsZero() {
		t.Fatal("the shared window was not adopted locally")
	}
}

// A failing shared store must not break the request: the local view keeps
// working and the picker is unchanged.
func TestSharedModelWindowFailureFallsBackToLocal(t *testing.T) {
	up := newRawUpstream(t, func(n int, r rawReq, c net.Conn) { sseOK(c) })
	f := newFixture(t, up, accSpec{}, accSpec{})
	f.h.ModelWindows = &fakeWindows{err: context.DeadlineExceeded}

	code, body, _ := f.post(t, `{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if code != 200 {
		t.Fatalf("a broken shared store must not fail requests: %d %s", code, body)
	}
	if f.pool.Get("acct-0").ModelCoolUntil("glm-5.3", time.Now()).IsZero() == false {
		t.Fatal("no local window may be created out of an error")
	}
}
