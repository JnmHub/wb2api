package openai

import (
	"net"
	"testing"
)

// A key restricted to some accounts must never touch the others, and the
// restriction survives sticky affinity (the fallback path excludes them too).
func TestKeyAccountWhitelistNarrowsThePool(t *testing.T) {
	seen := map[string]int{}
	up := newRawUpstream(t, func(i int, r rawReq, c net.Conn) { seen[tokOf(r)]++; sseOK(c) })
	f := newFixture(t, up, accSpec{}, accSpec{})
	f.setSettings(t, `{"api_keys":[{"id":"k1","name":"only-first","key":"sk-first","enabled":true,"accounts":["acct-0"]}]}`)
	auth := map[string]string{"Authorization": "Bearer sk-first"}
	for i := 0; i < 8; i++ {
		code, body, _ := f.post(t, conv(500+i), auth)
		if code != 200 {
			t.Fatalf("request %d: %d %s", i, code, body)
		}
	}
	if seen["Bearer tok-1"] != 0 {
		t.Fatalf("a whitelisted key reached the excluded account: %v", seen)
	}
	if seen["Bearer tok-0"] != 8 {
		t.Fatalf("the allowed account served %d of 8 requests: %v", seen["Bearer tok-0"], seen)
	}
	// A key with no patterns is unrestricted.
	f.setSettings(t, `{"api_keys":[{"id":"k1","name":"open","key":"sk-open","enabled":true}]}`)
	open := map[string]string{"Authorization": "Bearer sk-open"}
	for i := 0; i < 6; i++ {
		if code, body, _ := f.post(t, conv(900+i), open); code != 200 {
			t.Fatalf("unrestricted key request %d: %d %s", i, code, body)
		}
	}
	if seen["Bearer tok-1"] == 0 {
		t.Fatalf("an unrestricted key must still use every account: %v", seen)
	}
}
