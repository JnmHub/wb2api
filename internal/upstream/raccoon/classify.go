package raccoon

import (
	"encoding/json"
	"strings"
	"time"
)

// Class is the action class of one upstream failure. The gateway maps it onto
// the pool's existing machinery; the channel itself must not invent new
// scheduling semantics.
type Class int

const (
	// ClassOK means the response is not an error.
	ClassOK Class = iota
	// ClassTokenExpired asks for one refresh + retry on the same account, and
	// falls to "needs login" when the credential cannot be renewed.
	ClassTokenExpired
	// ClassQuota means the account's credit is gone. Credits are an
	// account-level budget, so the account is parked -- NOT a model cooldown.
	ClassQuota
	// ClassRateLimited is a short-lived throttle: cooldown the model, keep the
	// account.
	ClassRateLimited
	// ClassBlocked is the upstream content review. The account is healthy: a
	// neutral prompt retry is the right move, never a cooldown.
	ClassBlocked
	// ClassFatal is everything else: surface the upstream error as-is.
	ClassFatal
)

// Verdict is one classification result.
type Verdict struct {
	Class   Class
	Code    int
	Message string
	// Reset is when a rate-limited model may be retried (zero = unknown).
	Reset time.Time
}

// BlockedMarkers are the phrases the upstream's content review answers with.
var BlockedMarkers = []string{
	"blocked by security policy",
	"unapproved channel",
	"illegal api invocation",
}

var quotaCodes = map[string]bool{
	"insufficient_points": true,
	"insufficient_quota":  true,
	"arrearage":           true,
}

var quotaMarkers = []string{
	"insufficient_points", "insufficient quota", "积分不足", "余额不足", "欠费",
}

var rateMarkers = []string{
	"rate limit", "too many requests", "请求过于频繁", "频率", "rpm", "tpm",
}

// Classify maps one upstream error onto an action class. body is the raw
// response (may be empty or non-JSON); status is the HTTP status.
func Classify(status int, body []byte, now time.Time) Verdict {
	out := Verdict{}
	codeText := ""
	if len(body) > 0 {
		obj := map[string]any{}
		if err := json.Unmarshal(body, &obj); err == nil {
			out.Message = envelopeMessage(obj)
			if code, ok := businessCode(obj); ok {
				out.Code = code
			}
			codeText = strings.ToLower(codeTextOf(obj))
			if out.Message == "" {
				out.Message = strings.TrimSpace(string(body))
			}
		} else {
			out.Message = strings.TrimSpace(string(body))
		}
	}
	lowered := strings.ToLower(out.Message)

	if status == 0 {
		// transport-level problem; the caller handles it
		out.Class = ClassFatal
		return out
	}
	if status == 401 || out.Code == CodeTokenExpired || out.Code == CodeTokenExpiredAlt ||
		strings.Contains(lowered, "authorization_verify_error") ||
		codeText == "authorization_verify_error" {
		out.Class = ClassTokenExpired
		return out
	}

	quota := out.Code == CodeInsufficientPoints || quotaCodes[codeText] || containsAny(lowered, quotaMarkers)
	if quota {
		out.Class = ClassQuota
		return out
	}
	if status == 429 {
		transient := codeText == "rate_limit" || containsAny(lowered, rateMarkers)
		out.Class = ClassRateLimited
		if transient {
			out.Reset = now.Add(time.Minute)
		}
		return out
	}
	if status >= 400 && containsAny(lowered, BlockedMarkers) {
		out.Class = ClassBlocked
		return out
	}
	if status < 400 {
		out.Class = ClassOK
		return out
	}
	out.Class = ClassFatal
	return out
}

// codeTextOf reads the "code" field as text: the upstream uses a string
// ("rate_limit", "insufficient_points") on some errors and a number on others.
func codeTextOf(body map[string]any) string {
	for _, v := range []any{body["code"], pathOf(body, "error", "code")} {
		switch t := v.(type) {
		case string:
			return strings.TrimSpace(t)
		}
	}
	return ""
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, strings.ToLower(n)) {
			return true
		}
	}
	return false
}
