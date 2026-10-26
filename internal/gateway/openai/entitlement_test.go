package openai

import (
	"net"
	"testing"
	"time"

	"wb2api/internal/store/accounts"
)

// 11158 means "this model is not available to this account", so the account
// stops being picked for it and another account gets a chance. The account's
// other models keep working, and the block survives a restart because it is
// written to the record.
func TestModelEntitlementErrorBlocksOnlyThatAccount(t *testing.T) {
	var seen []string
	up := newRawUpstream(t, func(i int, r rawReq, c net.Conn) {
		tok := tokOf(r)
		seen = append(seen, tok)
		if len(seen) == 1 {
			status(c, 500, `{"code":11158,"msg":"model not available to this account"}`)
			return
		}
		sseOK(c)
	})
	f := newFixture(t, up, accSpec{}, accSpec{})
	hdr := map[string]string{"X-Conversation-Id": "ent-1"}
	code, _, _ := f.post(t, conv(1), hdr)
	if code != 200 {
		t.Fatalf("the second account must serve the request, got %d", code)
	}
	reqs := up.requests()
	if len(reqs) != 2 {
		t.Fatalf("upstream calls = %d; want the first account to be refused and the second to serve", len(reqs))
	}
	if tokOf(reqs[0]) == tokOf(reqs[1]) {
		t.Fatalf("the blocked account must not be retried: %v", seen)
	}
	f.pmu.Lock()
	var blocked *accounts.Record
	for _, r := range f.persisted {
		if len(r.ModelCool) == 1 {
			blocked = r
		}
	}
	f.pmu.Unlock()
	if blocked == nil {
		t.Fatal("the 11158 block was not persisted, so a restart would forget it")
	}
	for model, until := range blocked.ModelCool {
		if model != "glm-5.3" {
			t.Fatalf("blocked model = %q; want glm-5.3", model)
		}
		left := time.Until(time.Unix(0, int64(until*1e9)))
		if left < 23*time.Hour || left > 25*time.Hour {
			t.Fatalf("entitlement block lasts %v; want ~24h", left)
		}
	}
}

// A model-scoped 5xx that is not about entitlement keeps today's behaviour: it
// stops rotating instead of burning every account on the same dead model.
func TestModelScopedErrorStillStopsRotation(t *testing.T) {
	var n int
	up := newRawUpstream(t, func(i int, r rawReq, c net.Conn) {
		n++
		status(c, 500, `{"code":11134,"msg":"model provider temporarily unavailable"}`)
	})
	f := newFixture(t, up, accSpec{}, accSpec{})
	code, _, _ := f.post(t, conv(2), nil)
	if n != 1 {
		t.Fatalf("model-scoped error rotated accounts %d times; want 1", n)
	}
	if code == 200 {
		t.Fatal("a dead model must not be reported as success")
	}
}
