package openai

import (
	"net"
	"strings"
	"testing"
	"time"
)

// Regression for the field report: a model that is manually disabled on the
// only healthy account still counted as "servable" by the chain predicate, so
// the request ended in 503 "no usable account" instead of walking to the
// fallback model. The predicate must use the same readiness rule as the picker.
func TestModelChainAdvancesWhenTheModelIsDisabledOnEveryAccount(t *testing.T) {
	const flash, bunny = "deepseek-v4.1-flash", "space-bunny"
	up := newRawUpstream(t, func(n int, r rawReq, c net.Conn) {
		if strings.Contains(string(r.body), bunny) {
			sseOK(c)
			return
		}
		t.Errorf("upstream saw the disabled model: %s", string(r.body))
		status(c, 400, `{"error":{"message":"model disabled"}}`)
	})
	// acct-0: restricted upstream. acct-1: healthy, but flash is disabled on it.
	f := newFixture(t, up,
		accSpec{"restrictedUntil": float64(time.Now().Add(time.Hour).Unix())},
		accSpec{"disabledModels": []string{flash}},
	)
	if err := f.h.Settings.SetModelFallbacks(map[string][]string{flash: {bunny}}); err != nil {
		t.Fatal(err)
	}

	code, out, _ := f.post(t, `{"model":"`+flash+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if code != 200 {
		t.Fatalf("chain did not rescue an empty selection: %d %s", code, out)
	}
	if !strings.Contains(out, `"model":"`+flash+`"`) {
		t.Fatalf("response must echo the requested model:\n%s", out)
	}
	if len(up.requests()) != 1 {
		t.Fatalf("expected exactly one upstream call, got %d", len(up.requests()))
	}
}

// A model window (not just a manual deny list) must also trigger the chain:
// the account stays healthy, the model is simply unavailable on it.
func TestModelChainAdvancesOnCoolingAccountsWithNoUsableAccount(t *testing.T) {
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
		if _, ok := a.NoteModelCap(flash, now.Add(30*time.Minute), now); !ok {
			t.Fatalf("could not park %s for %s", a.Record().UID, flash)
		}
	}

	code, out, _ := f.post(t, `{"model":"`+flash+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if code != 200 {
		t.Fatalf("cooling model did not advance the chain: %d %s", code, out)
	}
}
