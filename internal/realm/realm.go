// Package realm holds the two upstream "exits" (international / China) and the
// pure helpers that decide which one a token, domain or model belongs to.
// It is a port of the realm-related parts of legacy wb_accounts.py and
// wb_identity.py.
package realm

import (
	"os"
	"strings"
)

const (
	Intl = "intl"
	CN   = "cn"
)

// Config describes one upstream exit.
type Config struct {
	Name            string
	ChatUpstream    string
	BillingUpstream string
	Origin          string
	Domain          string
	InfoFilename    string
	CacheDirName    string
	HasCheckin      bool
}

// DefaultCNChatBase is the China exit's chat-side default base URL.
const DefaultCNChatBase = "https://copilot.tencent.com"

// CNChatBase resolves WB_CN_CHAT_UPSTREAM. ignored is the raw env value when it
// was present but not an http(s) URL (the caller logs a warning).
func CNChatBase() (base string, ignored string) {
	raw := strings.TrimSpace(os.Getenv("WB_CN_CHAT_UPSTREAM"))
	if raw == "" {
		return DefaultCNChatBase, ""
	}
	b := strings.TrimRight(raw, "/")
	scheme, host, ok := strings.Cut(b, "://")
	if !ok || host == "" || (scheme != "http" && scheme != "https") {
		return DefaultCNChatBase, raw
	}
	return b, ""
}

// Get returns the config for a realm; unknown realms fall back to Intl, as in
// the legacy get_realm_config.
func Get(r string) Config {
	if r == CN {
		base, _ := CNChatBase()
		return Config{
			Name:            "国内版 (China)",
			ChatUpstream:    base,
			BillingUpstream: "https://www.codebuddy.cn",
			Origin:          "https://www.codebuddy.cn",
			Domain:          "copilot.tencent.com",
			InfoFilename:    "workbuddy-desktop.info",
			CacheDirName:    ".workbuddy",
			HasCheckin:      true,
		}
	}
	return Config{
		Name:            "国际版 (Global)",
		ChatUpstream:    "https://www.workbuddy.ai",
		BillingUpstream: "https://www.workbuddy.ai",
		Origin:          "https://www.workbuddy.ai",
		Domain:          "www.workbuddy.ai",
		InfoFilename:    "workbuddy-desktop-ai.info",
		CacheDirName:    ".workbuddy-ai",
		HasCheckin:      false,
	}
}

var (
	cnMarkers   = []string{"copilot.tencent.com", "codebuddy.cn", "workbuddy.cn"}
	intlMarkers = []string{"workbuddy.ai", "codebuddy.ai"}
)

func hasAny(s string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// Evidence inspects the token issuer, then the domain, for a realm hint.
// It returns "" when nothing identifies a realm (never guess).
func Evidence(token, domain string) string {
	iss := strings.ToLower(JWTIssuer(token))
	if hasAny(iss, cnMarkers) {
		return CN
	}
	if hasAny(iss, intlMarkers) {
		return Intl
	}
	dom := strings.ToLower(domain)
	if hasAny(dom, cnMarkers) {
		return CN
	}
	if hasAny(dom, intlMarkers) {
		return Intl
	}
	return ""
}

// Detect is Evidence with an Intl default.
func Detect(token, domain string) string {
	if e := Evidence(token, domain); e != "" {
		return e
	}
	return Intl
}

// DomainFor aligns a stored domain with the realm: a domain that names the
// other realm is replaced by this realm's canonical domain.
func DomainFor(r, domain string) string {
	dom := strings.TrimSpace(domain)
	other := cnMarkers
	if r == CN {
		other = intlMarkers
	}
	if dom != "" && !hasAny(strings.ToLower(dom), other) {
		return dom
	}
	return Get(r).Domain
}

// freeCapModels are the models whose 6004 window cap is persisted.
var freeCapModels = map[string]map[string]bool{
	Intl: {"hy4-preview-f": true, "hy3": true, "deepseek-v4.1-flash": true},
	CN:   {"hy4-preview-f": true, "hy3": true},
}

// IsFreeCapModel reports whether (realm, model) takes part in cap persistence.
func IsFreeCapModel(r, model string) bool {
	return model != "" && freeCapModels[r][model]
}

// NormaliseDisabledModels normalises a manual model deny-list: non-empty
// strings only, trimmed and lower-cased, de-duplicated, order preserved. Any
// non-list input yields an empty list.
func NormaliseDisabledModels(raw any) []string {
	out := []string{}
	list, ok := raw.([]any)
	if !ok {
		if sl, ok2 := raw.([]string); ok2 {
			list = make([]any, len(sl))
			for i, s := range sl {
				list[i] = s
			}
		} else {
			return out
		}
	}
	seen := map[string]bool{}
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			continue
		}
		m := strings.ToLower(strings.TrimSpace(s))
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

var intlOnly = map[string]bool{
	"gpt-6-astra": true, "gpt-5.6-sol": true, "gpt-5.6-terra": true, "gpt-5.6-luna": true,
	"gpt-5.5": true, "gpt-5.4": true, "gpt-5.3-codex": true, "gemini-3.5-flash": true, "grok-4.7": true,
}

var cnOnly = map[string]bool{
	"deepseek-v4-pro": true, "minimax-m3": true, "minimax-m2.7": true, "minimax-m2.5": true,
	"glm-5.1": true, "glm-5.0-turbo": true, "glm-4.6v": true,
	"kimi-k3-1": true, "kimi-k2.7": true, "kimi-k2-thinking": true,
	"hy3-x": true, "hy4-preview-dev": true, "hy4-preview-x": true,
}

// DetectModelRealm maps a model id to the exit that serves it; models served by
// both exits resolve to current (the default / key-bound realm).
func DetectModelRealm(model, current string) string {
	if model == "" {
		return current
	}
	m := strings.ToLower(model)
	if intlOnly[m] || strings.HasPrefix(m, "gpt-") || strings.HasPrefix(m, "gemini-") {
		return Intl
	}
	if cnOnly[m] || strings.HasPrefix(m, "minimax-") || strings.HasPrefix(m, "deepseek-v4-pro") {
		return CN
	}
	return current
}

var intlExclusive = map[string]bool{
	"gpt-6-astra": true, "gpt-5.6-sol": true, "gpt-5.6-terra": true, "gpt-5.6-luna": true,
	"gpt-5.5": true, "gpt-5.4": true, "gpt-5.3-codex": true, "gemini-3.5-flash": true, "grok-4.7": true,
}

var cnExclusive = map[string]bool{
	"deepseek-v4-pro": true, "glm-5.1": true, "glm-5v-turbo": true,
	"kimi-k3-1": true, "kimi-k2.7": true, "minimax-m3": true,
	"hy3-x": true, "hy4-preview-dev": true, "hy4-preview-x": true,
}

// ExclusiveRealm is the exit that alone serves a model ("" when both do).
// Sending gpt-6-astra to the domestic exit (or deepseek-v4-pro to the
// international one) earns an opaque 403, so callers reject it up front.
func ExclusiveRealm(model string) string {
	if model == "" {
		return ""
	}
	m := strings.ToLower(model)
	if intlExclusive[m] || strings.HasPrefix(m, "gpt-") || strings.HasPrefix(m, "gemini-") {
		return Intl
	}
	if cnExclusive[m] || strings.HasPrefix(m, "minimax-") || strings.HasPrefix(m, "deepseek-v4-pro") {
		return CN
	}
	return ""
}
