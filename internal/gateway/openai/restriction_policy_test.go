package openai

import (
	"net"
	"strings"
	"testing"
	"time"

	"wb2api/internal/risk"
)

// Default policy: a 403 account restriction tags that account and the request
// keeps going on the rest of the pool. The gateway itself must not stop.
func TestRestrictionTagsAccountAndKeepsServing(t *testing.T) {
	t.Setenv("WB_RESTRICTION_HOLD_SECONDS", "")
	t.Setenv("WB_RESTRICTED_403_ROTATE_MAX", "")
	up := newRawUpstream(t, func(n int, r rawReq, c net.Conn) {
		if strings.Contains(strings.Join(r.head, "\n"), "Bearer tok-0") {
			status(c, 403, restricted403)
			return
		}
		sseOK(c)
	})
	f := newFixture(t, up, accSpec{}, accSpec{})
	f.h.Risk = risk.NewGuard()

	code, body, hdr := f.post(t, chatBody, nil)
	if code != 200 {
		t.Fatalf("a restricted account must not fail the request: %d %s", code, body)
	}
	if hdr.Get("Retry-After") != "" {
		t.Fatalf("no realm pause expected, got Retry-After %q", hdr.Get("Retry-After"))
	}
	if rem := f.h.RiskHold("intl"); rem != 0 {
		t.Fatalf("realm must keep running, paused for %v", rem)
	}

	f.pmu.Lock()
	nRecs := len(f.persisted)
	var until float64
	if nRecs > 0 {
		until = f.persisted[0].RestrictedUntil
	}
	f.pmu.Unlock()
	if nRecs != 1 {
		t.Fatalf("the restricted account must be persisted once, got %d", nRecs)
	}
	if until < float64(time.Now().Add(time.Hour).Unix()) {
		t.Fatalf("restriction tag should last ~2h, got %v", until)
	}

	// The tagged account is skipped from now on: only the healthy one is used.
	before := len(up.requests())
	if code, body, _ := f.post(t, chatBody, nil); code != 200 {
		t.Fatalf("second request: %d %s", code, body)
	}
	after := up.requests()
	if len(after) != before+1 {
		t.Fatalf("one upstream call expected, got %d", len(after)-before)
	}
	if strings.Contains(strings.Join(after[len(after)-1].head, "\n"), "Bearer tok-0") {
		t.Fatal("a tagged account must not be picked again")
	}
}
