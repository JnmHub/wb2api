package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Limit keys: guards that share one shape — a global default plus an optional
// per-realm override (nil = inherit the global value, distinct from an explicit
// 0 = off for that realm).
const (
	LimitReserveCredits   = "reserve_credits"
	LimitDailyToken       = "daily_token_limit"
	LimitDailyCredit      = "daily_credit_limit"
	LimitModelDailyToken  = "model_daily_token_limit"
	LimitExpiringWindow   = "expiring_window_days"
	limitsKey             = "limits"
	defaultExpiringWindow = 7
)

// LimitKeys lists every known guard, in display order.
var LimitKeys = []string{LimitReserveCredits, LimitDailyToken, LimitDailyCredit, LimitModelDailyToken, LimitExpiringWindow}

// LimitEntry is one guard: the global default and the optional realm overrides.
type LimitEntry struct {
	Global int  `json:"global"`
	Intl   *int `json:"intl"`
	CN     *int `json:"cn"`
}

// Effective is the value for a realm ("" or unknown = the global default).
func (e LimitEntry) Effective(realm string) int {
	switch realm {
	case "intl":
		if e.Intl != nil {
			return *e.Intl
		}
	case "cn":
		if e.CN != nil {
			return *e.CN
		}
	}
	return e.Global
}

func defaultGlobal(key string) int {
	if key == LimitExpiringWindow {
		return defaultExpiringWindow
	}
	return 0
}

func coerceGlobal(v any) int {
	n := intOf(v)
	if n < 0 {
		return 0
	}
	return n
}

func coerceOverride(v any) *int {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if strings.TrimSpace(t) == "" {
			return nil
		}
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return nil
		}
		if n < 0 {
			n = 0
		}
		return &n
	}
	n := intOf(v)
	if n < 0 {
		n = 0
	}
	return &n
}

func normalizeLimits(raw map[string]any) map[string]LimitEntry {
	out := make(map[string]LimitEntry, len(LimitKeys))
	for _, key := range LimitKeys {
		entry, _ := raw[key].(map[string]any)
		g := defaultGlobal(key)
		if gv, ok := entry["global"]; ok && gv != nil {
			g = coerceGlobal(gv)
		}
		out[key] = LimitEntry{Global: g, Intl: coerceOverride(entry["intl"]), CN: coerceOverride(entry["cn"])}
	}
	return out
}

// Limits returns every guard, filling in missing keys. A pre-grouping file with
// flat top-level keys is read as the global values.
func (s *Store) Limits() map[string]LimitEntry {
	d := s.View()
	if raw, ok := d[limitsKey].(map[string]any); ok {
		return normalizeLimits(raw)
	}
	out := make(map[string]LimitEntry, len(LimitKeys))
	for _, key := range LimitKeys {
		g := defaultGlobal(key)
		if v, ok := d[key]; ok {
			g = coerceGlobal(v)
		}
		out[key] = LimitEntry{Global: g}
	}
	return out
}

// Limit is one guard's effective value for realm.
func (s *Store) Limit(key, realm string) int { return s.Limits()[key].Effective(realm) }

// SetLimit stores one guard at one scope ("global", "intl" or "cn"). A nil value
// clears an override back to "inherit"; the global slot always stores a number.
func (s *Store) SetLimit(key, scope string, value *int) (LimitEntry, error) {
	known := false
	for _, k := range LimitKeys {
		if k == key {
			known = true
		}
	}
	if !known {
		return LimitEntry{}, fmt.Errorf("unknown limit: %s", key)
	}
	if scope != "global" && scope != "intl" && scope != "cn" {
		return LimitEntry{}, fmt.Errorf("unknown scope: %s", scope)
	}
	var out LimitEntry
	err := s.Update(func(d Doc) error {
		var cur map[string]LimitEntry
		if raw, ok := d[limitsKey].(map[string]any); ok {
			cur = normalizeLimits(raw)
		} else {
			cur = map[string]LimitEntry{}
			for _, k := range LimitKeys {
				g := defaultGlobal(k)
				if v, ok := d[k]; ok {
					g = coerceGlobal(v)
					delete(d, k)
				}
				cur[k] = LimitEntry{Global: g}
			}
		}
		e := cur[key]
		switch scope {
		case "global":
			if value == nil {
				e.Global = 0
			} else if *value < 0 {
				e.Global = 0
			} else {
				e.Global = *value
			}
		case "intl":
			e.Intl = clampOverride(value)
		case "cn":
			e.CN = clampOverride(value)
		}
		cur[key] = e
		grouped := make(map[string]any, len(cur))
		for k, v := range cur {
			m := map[string]any{"global": v.Global, "intl": nil, "cn": nil}
			if v.Intl != nil {
				m["intl"] = *v.Intl
			}
			if v.CN != nil {
				m["cn"] = *v.CN
			}
			grouped[k] = m
		}
		d[limitsKey] = grouped
		out = e
		return nil
	})
	return out, err
}

func clampOverride(v *int) *int {
	if v == nil {
		return nil
	}
	n := *v
	if n < 0 {
		n = 0
	}
	return &n
}

// ---------------------------------------------------------------- switches

func (d Doc) flag(key string) bool { b, _ := d[key].(bool); return b }

// flagOr reads a switch with an explicit default for installs that never
// toggled it.
func (d Doc) flagOr(key string, def bool) bool {
	if b, ok := d[key].(bool); ok {
		return b
	}
	return def
}

func (d Doc) intOr(key string, def, lo, hi int) int {
	var n int
	switch v := d[key].(type) {
	case float64:
		n = int(v)
	case int:
		n = v
	case int64:
		n = int(v)
	case json.Number:
		i, err := v.Int64()
		if err != nil {
			return def
		}
		n = int(i)
	default:
		return def
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// AutoSwitchProduct: may an upstream 429 rotate an account's outbound identity
// (off unless the operator turns it on).
func (s *Store) AutoSwitchProduct() bool { return s.View().flag("auto_switch_product") }

var autoSwitchAll = []string{"workbuddy", "vscode", "cli"}

// AutoSwitchProducts lists the identities a 429 switch may move to (all three
// when unset; unknown entries are dropped).
func (s *Store) AutoSwitchProducts() []string {
	raw, ok := s.View()["auto_switch_products"].([]any)
	if !ok {
		return append([]string(nil), autoSwitchAll...)
	}
	var out []string
	for _, it := range raw {
		str, _ := it.(string)
		for _, a := range autoSwitchAll {
			if str == a {
				dup := false
				for _, o := range out {
					dup = dup || o == a
				}
				if !dup {
					out = append(out, a)
				}
			}
		}
	}
	return out
}

// EgressBackoff: account transient failures per outbound path (seconds-level
// backoff) instead of feeding the account breaker.
func (s *Store) EgressBackoff() bool { return s.View().flag("egress_backoff_enabled") }

// FreeModelCreditFallback: retry a fully capped free model on its paid sibling.
func (s *Store) FreeModelCreditFallback() bool { return s.View().flag("free_model_credit_fallback") }

// AuthDisabled: API-key checking switched off entirely.
func (s *Store) AuthDisabled() bool { return s.View().flag("auth_disabled") }

// RemainingPriority: dispatch by the remaining-usage estimate (opt-in).
func (s *Store) RemainingPriority() bool { return s.View().flag("remaining_priority_enabled") }

// LocalWebTools: execute Responses web_search / web_fetch gateway-side.
func (s *Store) LocalWebTools() bool { return s.View().flag("local_web_tools") }

// DailyChatWeb: also run the INTL web-console agent conversation (the leg that
// actually counts for the daily activity bonus). Default on.
func (s *Store) DailyChatWeb() bool { return s.View().flagOr("daily_chat_web", true) }

// DailyChatWebRetry: retry a slow web turn, doubling the wait each round.
func (s *Store) DailyChatWebRetry() bool { return s.View().flagOr("daily_chat_web_retry", true) }

// DailyChatWebAttempts: total web turns including the first (1..5).
func (s *Store) DailyChatWebAttempts() int { return s.View().intOr("daily_chat_web_attempts", 3, 1, 5) }

// DailyChatWebTimeout: seconds the first web turn may take (30..600).
func (s *Store) DailyChatWebTimeout() int {
	def := 120
	if v := os.Getenv("WB_WEB_TURN_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			def = n
		}
	}
	return s.View().intOr("daily_chat_web_timeout", def, 30, 600)
}

// AnthropicMessages: the Messages compatibility layer is switched on.
func (s *Store) AnthropicMessages() bool { return s.View().flag("enable_anthropic_messages") }

func numberFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case interface{ Float64() (float64, error) }:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}
