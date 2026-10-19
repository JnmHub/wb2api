package settings

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newKeySecret() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return "wb-" + base64.RawURLEncoding.EncodeToString(b)
}

func stamp() string { return time.Now().Format("2006/01/02 15:04") }

// ----------------------------------------------------------------- API keys

// KeyInput describes a key to create or change. A nil field keeps the stored
// value (so a partial client never silently clears an exit binding or a model
// restriction).
type KeyInput struct {
	Name             *string
	Key              *string // a new secret; nil on create generates one
	Realm            *string
	Models           *[]string
	ConcurrencyLimit *int
	Enabled          *bool
}

func (in KeyInput) validate() error {
	if in.Key != nil && strings.TrimSpace(*in.Key) != "" && len(strings.TrimSpace(*in.Key)) < 4 {
		return errors.New("api key must be at least 4 characters")
	}
	if in.Realm != nil {
		switch strings.ToLower(strings.TrimSpace(*in.Realm)) {
		case "", "intl", "cn":
		default:
			return errors.New("realm must be intl, cn or empty")
		}
	}
	if in.ConcurrencyLimit != nil && *in.ConcurrencyLimit < 0 {
		return errors.New("concurrency_limit cannot be negative")
	}
	return nil
}

func rawKeys(d Doc) []any {
	l, _ := d["api_keys"].([]any)
	return l
}

func findKey(list []any, id string) (map[string]any, int) {
	for i, raw := range list {
		if m, ok := raw.(map[string]any); ok && fmt.Sprint(m["id"]) == id {
			return m, i
		}
	}
	return nil, -1
}

func applyKeyInput(m map[string]any, in KeyInput) {
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" {
			name = "未命名"
		}
		m["name"] = name
	}
	if in.Key != nil && strings.TrimSpace(*in.Key) != "" {
		m["key"] = strings.TrimSpace(*in.Key)
	}
	if in.Realm != nil {
		m["realm"] = strings.ToLower(strings.TrimSpace(*in.Realm))
	}
	if in.Models != nil {
		m["models"] = cleanModelPatterns(stringsToAny(*in.Models))
	}
	if in.ConcurrencyLimit != nil {
		m["concurrency_limit"] = *in.ConcurrencyLimit
	}
	if in.Enabled != nil {
		m["enabled"] = *in.Enabled
	}
}

func stringsToAny(l []string) []any {
	out := make([]any, len(l))
	for i, s := range l {
		out[i] = s
	}
	return out
}

// CreateKey adds an API key (generating the secret when none is given) and
// returns the stored entry, secret included — the one time it is shown.
func (s *Store) CreateKey(in KeyInput) (APIKey, error) {
	if err := in.validate(); err != nil {
		return APIKey{}, err
	}
	var id string
	err := s.Update(func(d Doc) error {
		if in.Key == nil || strings.TrimSpace(*in.Key) == "" {
			k := newKeySecret()
			in.Key = &k
		}
		list := rawKeys(d)
		for _, raw := range list {
			if m, ok := raw.(map[string]any); ok && fmt.Sprint(m["key"]) == strings.TrimSpace(*in.Key) && fmt.Sprint(m["deleted_at"]) == "" {
				return errors.New("that key already exists")
			}
		}
		id = randHex(6)
		m := map[string]any{"id": id, "name": "未命名", "key": "", "realm": "", "models": []any{},
			"concurrency_limit": 0, "enabled": true, "created_at": stamp(), "deleted_at": ""}
		applyKeyInput(m, in)
		d["api_keys"] = append(list, m)
		delete(d, "api_key")
		delete(d, "api_key_set")
		return nil
	})
	if err != nil {
		return APIKey{}, err
	}
	for _, k := range s.APIKeys(true) {
		if k.ID == id {
			return k, nil
		}
	}
	return APIKey{}, errors.New("key vanished after saving")
}

// UpdateKey changes an existing live key.
func (s *Store) UpdateKey(id string, in KeyInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	return s.Update(func(d Doc) error {
		m, _ := findKey(rawKeys(d), id)
		if m == nil {
			return fmt.Errorf("unknown key %q", id)
		}
		if fmt.Sprint(m["deleted_at"]) != "" {
			return errors.New("a deleted key cannot be changed")
		}
		applyKeyInput(m, in)
		return nil
	})
}

// DeleteKey retires a key: the row stays (the usage log attributes spend by id)
// but its secret is wiped, so it can never authenticate again.
func (s *Store) DeleteKey(id string) error {
	return s.Update(func(d Doc) error {
		m, _ := findKey(rawKeys(d), id)
		if m == nil {
			return fmt.Errorf("unknown key %q", id)
		}
		m["key"] = ""
		m["enabled"] = false
		m["deleted_at"] = stamp()
		return nil
	})
}

// RevealKey returns a live key's secret.
func (s *Store) RevealKey(id string) (string, bool) {
	for _, k := range s.APIKeys(false) {
		if k.ID == id && k.Key != "" {
			return k.Key, true
		}
	}
	return "", false
}

// ----------------------------------------------------------- proxy slots

// ProxySlot is a named outbound proxy.
type ProxySlot struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Enabled     bool   `json:"enabled"`
	IP          string `json:"ip"`
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
	IPType      string `json:"ip_type"`
	ISP         string `json:"isp"`
	ASN         string `json:"asn"`
	ProbedAt    int64  `json:"probed_at"`
}

func slotFromRaw(raw any) (ProxySlot, bool) {
	m, ok := raw.(map[string]any)
	if !ok {
		return ProxySlot{}, false
	}
	u := strings.TrimSpace(trimmedString(m["url"]))
	if u == "" {
		return ProxySlot{}, false
	}
	en := true
	if b, ok := m["enabled"].(bool); ok && !b {
		en = false
	}
	return ProxySlot{
		ID: trimmedString(m["id"]), Name: trimmedString(m["name"]), URL: u, Enabled: en,
		IP: trimmedString(m["ip"]), Country: trimmedString(m["country"]),
		CountryCode: strings.ToUpper(trimmedString(m["country_code"])), IPType: strings.ToLower(trimmedString(m["ip_type"])),
		ISP: trimmedString(m["isp"]), ASN: trimmedString(m["asn"]), ProbedAt: int64(intOf(m["probed_at"])),
	}, true
}

// ProxySlots lists the configured slots in stored order.
func (s *Store) ProxySlots() []ProxySlot {
	list, _ := s.View()["proxy_slots"].([]any)
	var out []ProxySlot
	seen := map[string]bool{}
	for _, raw := range list {
		sl, ok := slotFromRaw(raw)
		if !ok || seen[sl.URL] {
			continue
		}
		seen[sl.URL] = true
		if sl.ID == "" {
			sl.ID = fmt.Sprintf("slot-%d", len(out)+1)
		}
		out = append(out, sl)
	}
	return out
}

func validProxyURL(u string) error {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return errors.New("proxy must be a URL like http://host:port or socks5://host:port")
	}
	switch p.Scheme {
	case "http", "https", "socks5", "socks5h":
		return nil
	}
	return errors.New("proxy scheme must be http, https, socks5 or socks5h")
}

// SetProxySlots replaces the slot list. Ids come from a counter that only ever
// grows: accounts persist the id they are bound to, so recycling a freed id
// would silently re-point an account at a different exit.
func (s *Store) SetProxySlots(slots []ProxySlot) ([]ProxySlot, error) {
	for _, sl := range slots {
		if err := validProxyURL(strings.TrimSpace(sl.URL)); err != nil {
			return nil, err
		}
	}
	var out []ProxySlot
	err := s.Update(func(d Doc) error {
		seq := intOf(d["proxy_slot_seq"])
		old, _ := d["proxy_slots"].([]any)
		if len(old) > seq {
			seq = len(old)
		}
		bump := func(id string) {
			var n int
			if _, err := fmt.Sscanf(id, "slot-%d", &n); err == nil && n > seq {
				seq = n
			}
		}
		for _, raw := range old {
			if m, ok := raw.(map[string]any); ok {
				bump(trimmedString(m["id"]))
			}
		}
		for _, sl := range slots {
			bump(sl.ID)
		}
		seen := map[string]bool{}
		ids := map[string]bool{}
		var stored []any
		for _, sl := range slots {
			sl.URL = strings.TrimSpace(sl.URL)
			if sl.URL == "" || seen[sl.URL] {
				continue
			}
			seen[sl.URL] = true
			if sl.ID == "" || ids[sl.ID] {
				seq++
				sl.ID = fmt.Sprintf("slot-%d", seq)
			}
			ids[sl.ID] = true
			sl.CountryCode = strings.ToUpper(sl.CountryCode)
			sl.IPType = strings.ToLower(sl.IPType)
			out = append(out, sl)
			stored = append(stored, map[string]any{"id": sl.ID, "name": strings.TrimSpace(sl.Name), "url": sl.URL,
				"enabled": sl.Enabled, "ip": sl.IP, "country": sl.Country, "country_code": sl.CountryCode,
				"ip_type": sl.IPType, "isp": sl.ISP, "asn": sl.ASN, "probed_at": sl.ProbedAt})
		}
		d["proxy_slots"] = stored
		d["proxy_slot_seq"] = seq
		return nil
	})
	return out, err
}

// ------------------------------------------------------- simple groups

// SetUpstream validates and stores the upstream timeouts / device token group.
func (s *Store) SetUpstream(patch map[string]any) (UpstreamConfig, error) {
	clean := map[string]any{}
	for k, v := range patch {
		switch k {
		case "header_timeout_seconds", "idle_timeout_seconds":
			n := intOf(v)
			if n < 1 {
				return UpstreamConfig{}, fmt.Errorf("%s must be a whole number of seconds, at least 1", k)
			}
			if n > 24*3600 {
				return UpstreamConfig{}, fmt.Errorf("%s cannot be more than a day", k)
			}
			clean[k] = n
		case "device_token", "device_token_file":
			str, ok := v.(string)
			if !ok {
				return UpstreamConfig{}, fmt.Errorf("%s must be a string", k)
			}
			clean[k] = strings.TrimSpace(str)
		default:
			return UpstreamConfig{}, fmt.Errorf("unknown upstream setting %q", k)
		}
	}
	err := s.Update(func(d Doc) error {
		cur, _ := d["upstream"].(map[string]any)
		d["upstream"] = DeepMerge(cur, clean)
		return nil
	})
	return s.UpstreamConfig(), err
}

// SetPrompt validates and stores the prompt mode.
func (s *Store) SetPrompt(patch map[string]any) (PromptConfig, error) {
	cur := s.PromptConfig()
	mode, file := cur.Mode, cur.File
	for k, v := range patch {
		str, ok := v.(string)
		switch k {
		case "mode":
			if !ok {
				return cur, errors.New("prompt mode must be a string")
			}
			mode = strings.ToLower(strings.TrimSpace(str))
			if mode != "passthrough" && mode != "custom" && mode != "append" {
				return cur, errors.New("prompt mode must be passthrough, custom or append")
			}
		case "file":
			if !ok {
				return cur, errors.New("prompt file must be a string")
			}
			file = strings.TrimSpace(str)
		default:
			return cur, fmt.Errorf("unknown prompt setting %q", k)
		}
	}
	err := s.Update(func(d Doc) error {
		d["prompt"] = map[string]any{"mode": mode, "file": file}
		return nil
	})
	return PromptConfig{Mode: mode, File: file}, err
}

// Switches are the boolean toggles of the settings page.
var switchKeys = map[string]bool{
	"auto_switch_product": true, "egress_backoff_enabled": true, "free_model_credit_fallback": true,
	"enable_anthropic_messages": true, "auth_disabled": true, "remaining_priority_enabled": true,
	"local_web_tools": true, "log_db_enabled": true, "update_check_enabled": true,
}

// SetSwitch stores one boolean toggle.
func (s *Store) SetSwitch(key string, on bool) error {
	if !switchKeys[key] {
		return fmt.Errorf("unknown switch %q", key)
	}
	return s.Update(func(d Doc) error { d[key] = on; return nil })
}

// SetAutoSwitchProducts stores the identities a 429 switch may move to.
func (s *Store) SetAutoSwitchProducts(list []string) ([]string, error) {
	var out []string
	for _, p := range list {
		ok := false
		for _, a := range autoSwitchAll {
			ok = ok || a == p
		}
		if !ok {
			return nil, fmt.Errorf("unknown identity %q (workbuddy, vscode or cli)", p)
		}
		dup := false
		for _, o := range out {
			dup = dup || o == p
		}
		if !dup {
			out = append(out, p)
		}
	}
	err := s.Update(func(d Doc) error { d["auto_switch_products"] = stringsToAny(out); return nil })
	return out, err
}

// CreditsRefreshHours is the TTL after which a balance is refreshed in the
// background (0 = never).
func (s *Store) CreditsRefreshHours() float64 {
	v := s.View()["credits_refresh_hours"]
	switch t := v.(type) {
	case float64:
		if t >= 0 {
			return t
		}
	case int:
		if t >= 0 {
			return float64(t)
		}
	default:
		if f, ok := numberFloat(v); ok && f >= 0 {
			return f
		}
	}
	return 12
}

// SetCreditsRefreshHours stores the credits refresh TTL.
func (s *Store) SetCreditsRefreshHours(h float64) error {
	if h < 0 || h > 24*30 {
		return errors.New("credits_refresh_hours must be between 0 and 720")
	}
	return s.Update(func(d Doc) error { d["credits_refresh_hours"] = h; return nil })
}

// ---------------------------------------------------------------- schedule

// DefaultSchedule are the hours (local time) the unattended jobs fire at.
var DefaultSchedule = map[string][]int{
	"checkin":   {9, 21}, // CN daily check-in
	"travel":    {9, 21}, // kept for settings compatibility (unused)
	"keepalive": {22},    // token keep-alive sweep
	"cat":       {1},     // kept for settings compatibility (unused)
}

func normalizeHours(v any) ([]int, bool) {
	var raw []any
	switch t := v.(type) {
	case []any:
		raw = t
	case string:
		for _, p := range strings.Split(t, ",") {
			raw = append(raw, strings.TrimSpace(p))
		}
	case []int:
		for _, h := range t {
			raw = append(raw, h)
		}
	default:
		return nil, false
	}
	seen := map[int]bool{}
	var out []int
	for _, x := range raw {
		h := 0
		if s, ok := x.(string); ok {
			n, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil {
				continue
			}
			h = n
		} else {
			h = intOf(x)
		}
		if h < 0 || h > 23 || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	sortInts(out)
	return out, len(out) > 0
}

func sortInts(l []int) {
	for i := 1; i < len(l); i++ {
		for j := i; j > 0 && l[j] < l[j-1]; j-- {
			l[j], l[j-1] = l[j-1], l[j]
		}
	}
}

// ScheduleHours returns the configured grid; a missing or invalid entry falls
// back to its default, so a hand-edited file can never stop the scheduler.
func (s *Store) ScheduleHours() map[string][]int {
	raw, _ := s.View()["schedule"].(map[string]any)
	out := map[string][]int{}
	for k, def := range DefaultSchedule {
		if h, ok := normalizeHours(raw[k]); ok {
			out[k] = h
		} else {
			out[k] = append([]int(nil), def...)
		}
	}
	return out
}

// SetScheduleHours stores a partial update; a supplied key must hold at least
// one valid hour.
func (s *Store) SetScheduleHours(patch map[string]any) (map[string][]int, error) {
	for k, v := range patch {
		if _, known := DefaultSchedule[k]; !known {
			continue
		}
		if _, ok := normalizeHours(v); !ok {
			return nil, fmt.Errorf("schedule.%s must contain hours 0-23", k)
		}
	}
	err := s.Update(func(d Doc) error {
		cur, _ := d["schedule"].(map[string]any)
		merged := map[string]any{}
		for k := range DefaultSchedule {
			if v, ok := patch[k]; ok {
				h, _ := normalizeHours(v)
				merged[k] = h
			} else if h, ok := normalizeHours(cur[k]); ok {
				merged[k] = h
			} else {
				merged[k] = DefaultSchedule[k]
			}
		}
		d["schedule"] = merged
		return nil
	})
	return s.ScheduleHours(), err
}

// SchedulerEnabled: the background jobs run (default on).
func (s *Store) SchedulerEnabled() bool {
	b, ok := s.View()["scheduler_enabled"].(bool)
	return !ok || b
}

// SetSchedulerEnabled switches the background jobs.
func (s *Store) SetSchedulerEnabled(on bool) error {
	return s.Update(func(d Doc) error { d["scheduler_enabled"] = on; return nil })
}
