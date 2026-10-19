// Package ratelimit interprets upstream 429/403 bodies. It is a port of the
// legacy helpers parse_rate_limit_reset, rate_limit_is_*, credential_scope_phrase
// and restriction_is_account_level, pinned by golden vectors from that code.
package ratelimit

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// CapCode is the upstream's "window quota used up" answer (code 6004): the only
// 429 that carries a reset clock and counts as a quota verdict.
const CapCode = 6004

var softMarkers = []string{"usage exceeds frequency limit", "too many requests", "请求过于频繁"}

var softCodes = []int{14003}

// accountScopePhrases scope a 429 to the credential itself. ASCII phrases are
// anchored on word boundaries so an identifier like accountId cannot match.
var accountScopePhrases = []string{
	"account-level", "account level", "account-wide", "account scope",
	"account quota", "account rate limit", "account limit",
	"per account", "per-account", "this account", "your account",
	"the account", "an account",
	"credential-level", "credential level", "credential scope",
	"credential limit", "per credential", "this credential",
	"the credential", "your credential",
	"账号级", "账户级", "该账号", "此账号", "该账户", "此账户",
	"账号限流", "账户限流", "凭证级", "该凭证",
}

var scopeRes = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(accountScopePhrases))
	for i, p := range accountScopePhrases {
		if isASCII(p) {
			out[i] = regexp.MustCompile(`\b` + regexp.QuoteMeta(p) + `\b`)
		}
	}
	return out
}()

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// ScopePhrase returns the phrase that scopes the 429 to the credential, or "".
func ScopePhrase(detail string) string {
	lowered := strings.ToLower(detail)
	for i, p := range accountScopePhrases {
		if re := scopeRes[i]; re != nil {
			if re.MatchString(lowered) {
				return p
			}
		} else if strings.Contains(lowered, p) {
			return p
		}
	}
	return ""
}

// code returns the integer `code` of a JSON body (ok=false when absent, not
// JSON, or not an integer-like value).
func code(detail string) (int, bool) {
	var obj map[string]any
	dec := json.NewDecoder(strings.NewReader(detail))
	dec.UseNumber()
	if dec.Decode(&obj) != nil {
		return 0, false
	}
	switch v := obj["code"].(type) {
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return int(i), true
		}
		if f, err := v.Float64(); err == nil {
			return int(f), true
		}
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return i, true
		}
	}
	return 0, false
}

// IsSoftShape: the body names a rate limit, whatever its scope.
func IsSoftShape(detail string) bool {
	text := strings.ToLower(detail)
	for _, m := range softMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	if c, ok := code(detail); ok {
		for _, s := range softCodes {
			if c == s {
				return true
			}
		}
	}
	return false
}

// IsCap: the quota-cap answer (code 6004). Anything unparsable is not a cap.
func IsCap(detail string) bool {
	c, ok := code(detail)
	return ok && c == CapCode
}

// IsAccountLevel: positive evidence the 429 is credential-scoped. A reset clock
// means a model-scoped quota window; absence of one proves nothing.
func IsAccountLevel(detail string, hasReset bool) bool {
	if hasReset {
		return false
	}
	return ScopePhrase(detail) != ""
}

var (
	resetRe    = regexp.MustCompile(`(?:reset at|将在)\s*(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2})`)
	resetFall  = regexp.MustCompile(`(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2})\s*UTC[+-]\d{1,2}`)
	tzRe       = regexp.MustCompile(`UTC([+-]\d{1,2})(?::?(\d{2}))?`)
	restrictRe = regexp.MustCompile(`"code"\s*:\s*"?11140"?`)
)

var restrictionMarkers = []string{"unusual activity", "账号存在异常行为", "已被限制"}

// IsRestriction: a 403 body saying the ACCOUNT is restricted (11140), not the
// content. An incomplete body is judged conservatively (not a restriction).
func IsRestriction(detail string) bool {
	if detail == "" {
		return false
	}
	if restrictRe.MatchString(detail) {
		return true
	}
	lowered := strings.ToLower(detail)
	for _, m := range restrictionMarkers {
		if strings.Contains(lowered, m) {
			return true
		}
	}
	return false
}

// unescapeBody restores a JSON-escaped body (`将在`). Like the legacy
// encode("utf-8").decode("unicode_escape"), real non-ASCII text in the same
// body is mangled (bytes read as Latin-1); the fallback pattern below does not
// depend on it.
func unescapeBody(s string) string {
	if !strings.Contains(s, `\u`) {
		return s
	}
	b := []byte(s)
	var out []rune
	for i := 0; i < len(b); {
		c := b[i]
		if c != '\\' || i+1 >= len(b) {
			out = append(out, rune(c))
			i++
			continue
		}
		n := b[i+1]
		switch n {
		case 'u', 'U', 'x':
			width := map[byte]int{'u': 4, 'U': 8, 'x': 2}[n]
			if i+2+width > len(b) {
				return s // invalid escape: the legacy decoder raises, the body stays as it was
			}
			v, err := strconv.ParseUint(string(b[i+2:i+2+width]), 16, 32)
			if err != nil {
				return s
			}
			out = append(out, rune(v))
			i += 2 + width
		case 'n':
			out, i = append(out, '\n'), i+2
		case 't':
			out, i = append(out, '\t'), i+2
		case 'r':
			out, i = append(out, '\r'), i+2
		case '\\', '\'', '"':
			out, i = append(out, rune(n)), i+2
		case '/':
			out, i = append(out, '\\', '/'), i+2
		default:
			out, i = append(out, '\\', rune(n)), i+2
		}
	}
	return string(out)
}

// ParseReset pulls the quota reset instant out of a 429 body. Two shapes:
//
//	"... your usage will reset at 2026-09-19 18:29:03 UTC+8 ..."
//	"您的使用量已超出频率限制，将在 2026-10-02 16:00:34 UTC+8 重置 ..."
//
// The stamp is wall-clock time in the body's own UTC offset.
func ParseReset(detail string) (time.Time, bool) {
	if detail == "" {
		return time.Time{}, false
	}
	text := unescapeBody(detail)
	m := resetRe.FindStringSubmatch(text)
	if m == nil {
		m = resetFall.FindStringSubmatch(text)
	}
	if m == nil {
		return time.Time{}, false
	}
	stamp := strings.Replace(m[1], "T", " ", 1)
	offset := 0
	if tz := tzRe.FindStringSubmatch(text); tz != nil {
		hours, _ := strconv.Atoi(tz[1])
		minutes, _ := strconv.Atoi(tz[2])
		if hours >= 0 {
			offset = hours*3600 + minutes*60
		} else {
			offset = hours*3600 - minutes*60
		}
	}
	base, err := time.ParseInLocation("2006-01-02 15:04:05", stamp, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return base.Add(-time.Duration(offset) * time.Second), true
}
