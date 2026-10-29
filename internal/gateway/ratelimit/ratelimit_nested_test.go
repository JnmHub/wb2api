package ratelimit

import "testing"

// The upstream puts `code` in more than one place. The parser has to find it
// wherever it is nested, otherwise every code-based verdict (6004 cap, 14003
// soft limit, 11140 restriction, 14018 exhausted quota) silently degrades to
// plain text matching.
func TestCodeIsFoundNested(t *testing.T) {
	cases := []struct {
		name string
		body string
		cap  bool
	}{
		{"top level", `{"code":6004}`, true},
		{"string code", `{"code":"6004"}`, true},
		{"error.code", `{"error":{"code":6004}}`, true},
		{"error.data.code", `{"error":{"data":{"code":6004}}}`, true},
		{"data.code", `{"data":{"code":6004}}`, true},
		{"response.error.code", `{"response":{"error":{"code":6004}}}`, true},
		{"other nesting", `{"payload":{"inner":{"code":6004}}}`, true},
		{"not a cap", `{"error":{"data":{"code":14018}}}`, false},
	}
	for _, c := range cases {
		if got := IsCap(c.body); got != c.cap {
			t.Errorf("%s: IsCap(%s) = %v want %v", c.name, c.body, got, c.cap)
		}
	}
}

func TestQuotaExhausted(t *testing.T) {
	yes := []string{
		`{"error":{"data":{"code":14018,"msg":"额度已用尽，请访问以下链接，购买加量包以获取更多额度"}}}`,
		`{"code":14018}`,
		`{"code":"14018"}`,
		`{"msg":"额度已用尽"}`,
		`{"msg":"insufficient quota"}`,
		`{"msg":"quota exhausted for this billing cycle"}`,
	}
	for _, body := range yes {
		if !IsQuotaExhausted(body) {
			t.Errorf("IsQuotaExhausted(%q) = false want true", body)
		}
	}
	no := []string{
		`{"error":{"data":{"code":6004,"msg":"your usage will reset at 2026-09-19 18:29:03 UTC+8"}}}`,
		`{"code":14003,"msg":"too many requests"}`,
		`{"code":11140,"msg":"request illegal"}`,
		`{"msg":"额度重置成功"}`,
		``,
	}
	for _, body := range no {
		if IsQuotaExhausted(body) {
			t.Errorf("IsQuotaExhausted(%q) = true want false", body)
		}
	}
}

// A quota verdict is not a rate-limit verdict: the soft-limit ladder must not
// adopt the exhausted-quota body, or the account gets a short retry window.
func TestQuotaBodyIsNotASoftLimit(t *testing.T) {
	body := `{"error":{"data":{"code":14018,"msg":"额度已用尽，请访问以下链接，购买加量包以获取更多额度"}}}`
	if IsSoftShape(body) {
		t.Fatal("exhausted quota must not read as a frequency limit")
	}
	if IsAccountLevel(body, false) {
		t.Fatal("exhausted quota is parked by its own rule, not by scope phrases")
	}
}

func TestRestrictionFoundNested(t *testing.T) {
	body := `{"error":{"data":{"code":11140,"msg":"request illegal from an unapproved channel"}}}`
	if !IsRestriction(body) {
		t.Fatal("nested 11140 must still be a restriction")
	}
}
