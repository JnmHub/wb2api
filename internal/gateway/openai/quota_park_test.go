package openai

import (
	"net"
	"strings"
	"testing"
	"time"

	"wb2api/internal/pool"
)

// quota429Body is the shape the upstream really returns once an account's quota
// is gone: code 14018 nested under error.data, with a Chinese explanation.
const quota429Body = `{"error":{"type":"invalid_request_error","data":{"code":14018,` +
	`"msg":"额度已用尽，请访问以下链接，购买加量包以获取更多额度: https://www.codebuddy.cn/profile"}}}`

// An exhausted quota is not a rate limit: the account must be parked (visible
// as cooling) instead of getting a one-minute per-model window that the next
// request immediately burns again.
func TestQuotaExhausted429ParksTheWholeAccount(t *testing.T) {
	up := newRawUpstream(t, func(n int, r rawReq, c net.Conn) {
		if strings.Contains(strings.Join(r.head, "\n"), "Bearer tok-0") {
			status(c, 429, quota429Body)
			return
		}
		sseOK(c)
	})
	f := newFixture(t, up, accSpec{}, accSpec{})

	code, body, _ := f.post(t, chatBody, nil)
	if code != 200 {
		t.Fatalf("an exhausted account must not fail the request: %d %s", code, body)
	}

	now := time.Now()
	a0 := f.pool.Get("acct-0")
	if a0 == nil {
		t.Fatal("fixture lost acct-0")
	}
	if st := a0.State(now); st != pool.StatusCooling {
		t.Fatalf("exhausted account state = %s, want cooling", st)
	}
	if a0.HasModelCooldown(now) {
		t.Fatal("an exhausted quota must not be recorded as a per-model window")
	}

	// The parked account is no longer offered: only the healthy one serves.
	before := len(up.requests())
	if code, body, _ := f.post(t, chatBody, nil); code != 200 {
		t.Fatalf("second request: %d %s", code, body)
	}
	if after := up.requests(); len(after) != before+1 {
		t.Fatalf("one upstream call expected after parking, got %d", len(after)-before)
	}
}

// A plain 429 (no quota wording, no reset clock) keeps the old behaviour: a
// short per-model window, so a burst on one model does not park the account.
func TestOrdinary429StillCoolsOnlyTheModel(t *testing.T) {
	up := newRawUpstream(t, func(n int, r rawReq, c net.Conn) {
		if strings.Contains(strings.Join(r.head, "\n"), "Bearer tok-0") {
			status(c, 429, `{"error":{"message":"slow down"}}`)
			return
		}
		sseOK(c)
	})
	f := newFixture(t, up, accSpec{}, accSpec{})

	if code, body, _ := f.post(t, chatBody, nil); code != 200 {
		t.Fatalf("request: %d %s", code, body)
	}
	now := time.Now()
	a0 := f.pool.Get("acct-0")
	if !a0.HasModelCooldown(now) {
		t.Fatal("a plain 429 should park the model")
	}
	if st := a0.State(now); st == pool.StatusCooling {
		t.Fatalf("a model window must not park the whole account, state = %s", st)
	}
}
