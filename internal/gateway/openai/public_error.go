package openai

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"wb2api/internal/gateway/ratelimit"
)

// publicUpstreamDetail classifies untrusted upstream diagnostics into fixed
// public phrases. Redacting known token patterns is not sufficient: a server
// can echo arbitrary prompts, credentials, proxy URLs or private addresses.
// Scheduling still sees the original body; only the client-facing copy changes.
func publicUpstreamDetail(status int, body []byte) string {
	text := truncate(string(body), 4096)
	code := modelScopedErrorCode([]byte(text))
	known := map[int]string{
		11102: "model service not found",
		11128: "content rejected by upstream policy",
		11129: "invalid function parameters",
		11133: "invalid request parameters",
		11134: "model provider temporarily unavailable",
		11135: "invalid image data; replace the image",
		11140: "account has been restricted due to unusual activity",
		11150: "reasoning effort unsupported",
		11158: "model not available to this account",
	}
	if phrase := known[code]; phrase != "" {
		return fmt.Sprintf("%s (code %d)", phrase, code)
	}
	if status == http.StatusForbidden && ratelimit.IsRestriction(text) {
		return "account has been restricted due to unusual activity (code 11140)"
	}
	if status == http.StatusTooManyRequests {
		phrase := "rate limit reached"
		reset, hasReset := ratelimit.ParseReset(text)
		switch {
		case ratelimit.IsCap(text):
			phrase = "model quota reached (code 6004)"
		case ratelimit.IsAccountLevel(text, hasReset):
			phrase = "account rate limit reached"
		case ratelimit.IsSoftShape(text):
			phrase = "usage exceeds frequency limit"
		}
		if hasReset {
			phrase += "; reset at " + reset.UTC().Format(time.RFC3339)
		}
		return phrase
	}
	lower := strings.ToLower(text)
	has := func(parts ...string) bool {
		for _, part := range parts {
			if strings.Contains(lower, part) {
				return true
			}
		}
		return false
	}
	switch {
	case has("context length", "context_length", "prompt too long", "too many tokens", "maximum context"):
		return "request context exceeds the model's maximum context length"
	case has("no such model", "model not found", "unsupported model"):
		return "model not found or unsupported by upstream"
	case has("session") && has("not found", "expired"):
		return "account session expired at upstream"
	case status == http.StatusForbidden && has("waf"):
		return "upstream WAF rejected the request"
	case status == http.StatusForbidden:
		return "content rejected by upstream policy"
	case status == http.StatusUnauthorized:
		return "upstream authentication failed"
	case status == http.StatusPaymentRequired:
		return "insufficient account credits"
	case status == http.StatusBadRequest:
		return "invalid request parameters"
	case status == http.StatusRequestEntityTooLarge:
		return "request payload exceeds the upstream limit"
	case status >= 500:
		return "upstream service unavailable"
	default:
		return "upstream rejected the request"
	}
}
