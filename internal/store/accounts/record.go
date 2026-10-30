// Package accounts reads and writes the per-account credential files
// (accounts/<uid>.json). The on-disk format is shared with the legacy Python
// gateway, so a Go process can take over an existing directory and the Python
// one can still read what Go wrote.
//
// Only persisted fields live here. Runtime state (cooldowns, streaks, rate
// windows) belongs to the pool package.
package accounts

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"wb2api/internal/realm"
)

// ModelCap records an upstream "window quota exhausted" (code 6004) fact for
// one model: when it was hit and when the upstream says it resets.
type ModelCap struct {
	At    float64 `json:"at"`
	Reset float64 `json:"reset"`
}

// Record is the persisted shape of one account.
type Record struct {
	UID                string
	Nickname           string
	Domain             string
	Realm              string
	Platform           string
	Product            string
	EnterpriseID       string
	AccessToken        string
	RefreshToken       string
	ExpiresAt          int64
	AddedAt            float64
	Source             string
	ProxySlot          string
	ProxyLegacy        string
	Enabled            any // legacy files may hold any JSON value; nil/absent means true
	Weight             int
	Note               string
	DisabledModels     []string
	RestrictedUntil    float64
	Credits            map[string]any
	LastCheckin        any
	LastDailyChat      any
	LastActivityReport any
	ModelCaps          map[string]ModelCap
	// ModelCool holds model-scoped cooldowns (unix seconds) so a 429 window
	// survives a restart instead of being forgotten with the process' memory.
	ModelCool map[string]float64
	// ModelCoolWhy explains each ModelCool entry ("soft", "cap",
	// "entitlement", "manual"). Diagnostics only: nothing schedules from it.
	ModelCoolWhy map[string]string
	// TasksEnabled keeps an account out of scheduled chores (check-in, activity
	// report, daily chat, keepalive) while it keeps serving traffic. nil/absent
	// means enabled, so existing files behave exactly as before.
	TasksEnabled any
	// Tier is the cached billing tier ("free", "plus", …), informational only:
	// the gateway never derives permissions from it.
	Tier string

	// Extra preserves keys this version does not know about so a rewrite never
	// silently drops data written by a newer or hand-edited file.
	Extra map[string]any

	// Path is where the record was loaded from / last saved to.
	Path string

	// SharedBaseline is an immutable persisted snapshot used by shared-store
	// optimistic merges. It is runtime metadata and is never marshaled.
	SharedBaseline []byte
}

// IsEnabled applies Python truthiness to the stored `enabled` value.
func (r *Record) IsEnabled() bool {
	if r.Enabled == nil {
		return true
	}
	switch v := r.Enabled.(type) {
	case bool:
		return v
	case string:
		return v != ""
	case json.Number:
		f, err := v.Float64()
		return err != nil || f != 0
	case float64:
		return v != 0
	}
	return true
}

// IsEnterprise reports whether the account belongs to an enterprise space.
func (r *Record) IsEnterprise() bool {
	return r.EnterpriseID != "" || realm.JWTEnterpriseID(r.AccessToken) != ""
}

// TasksOn reports whether scheduled chores may run for this account. Legacy
// records without the key keep taking chores, exactly as before.
func (r *Record) TasksOn() bool {
	if r.TasksEnabled == nil {
		return true
	}
	switch v := r.TasksEnabled.(type) {
	case bool:
		return v
	case string:
		return v != "" && v != "0" && !strings.EqualFold(v, "false")
	case json.Number:
		f, err := v.Float64()
		return err != nil || f != 0
	case float64:
		return v != 0
	}
	return true
}

var knownKeys = map[string]bool{
	"uid": true, "nickname": true, "domain": true, "realm": true, "platform": true,
	"product": true, "enterpriseId": true, "accessToken": true, "refreshToken": true,
	"expiresAt": true, "addedAt": true, "source": true, "proxySlot": true, "proxy": true,
	"enabled": true, "weight": true, "note": true, "disabledModels": true,
	"restrictedUntil": true, "credits": true, "lastCheckin": true, "lastDailyChat": true,
	"lastActivityReport": true, "modelCaps": true, "modelCool": true, "modelCoolWhy": true,
	"tasksEnabled": true, "tier": true,
	// runtime-only: legacy files may still contain them; never trusted on load
	// and never written back.
	"lastError": true, "cooldownUntil": true,
}

// PyStr mimics Python's `str(x or "")` for decoded JSON scalars.
func pyStrOrEmpty(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return ""
	case json.Number:
		if f, err := t.Float64(); err == nil && f == 0 {
			return ""
		}
		return t.String()
	}
	return ""
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case float64:
		return t, true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}

func truthyAny(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case json.Number:
		f, err := t.Float64()
		return err != nil || f != 0
	case float64:
		return t != 0
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// FromMap builds a Record from decoded JSON, applying the same defaulting and
// normalisation rules as the legacy Account constructor. now supplies the
// default AddedAt.
func FromMap(data map[string]any, path string, now time.Time) *Record {
	if data == nil {
		data = map[string]any{}
	}
	r := &Record{Path: path, Extra: map[string]any{}}

	token := pyStrOrEmpty(data["accessToken"])
	r.AccessToken = token
	r.UID = pyStrOrEmpty(data["uid"])
	if r.UID == "" {
		r.UID = realm.JWTUID(token)
	}
	if nick, ok := data["nickname"].(string); ok && !strings.Contains(nick, "$wbEncrypted") {
		r.Nickname = strings.TrimSpace(nick)
	}
	r.Domain = pyStrOrEmpty(data["domain"])
	r.Realm = pyStrOrEmpty(data["realm"])
	if r.Realm == "" {
		r.Realm = realm.Detect(token, r.Domain)
	}
	if r.Domain == "" {
		r.Domain = realm.Get(r.Realm).Domain
	}
	r.Platform = pyStrOrEmpty(data["platform"])
	if r.Platform == "" {
		r.Platform = "CLI"
	}
	r.Product = NormalizeProduct(data["product"])
	r.EnterpriseID = pyStrOrEmpty(data["enterpriseId"])
	r.RefreshToken = pyStrOrEmpty(data["refreshToken"])
	r.ExpiresAt = realm.NormalizeEpoch(data["expiresAt"])
	if r.ExpiresAt == 0 {
		r.ExpiresAt = realm.JWTExp(token)
	}
	if r.EnterpriseID == "" {
		r.EnterpriseID = realm.JWTEnterpriseID(token)
	}
	if f, ok := toFloat(data["addedAt"]); ok && f != 0 {
		r.AddedAt = f
	} else {
		r.AddedAt = float64(now.UnixNano()) / 1e9
	}
	r.Source = pyStrOrEmpty(data["source"])
	if r.Source == "" {
		r.Source = "oauth"
	}
	r.ProxySlot = strings.TrimSpace(pyStrOrEmpty(data["proxySlot"]))
	r.ProxyLegacy = strings.TrimSpace(pyStrOrEmpty(data["proxy"]))

	if v, present := data["enabled"]; present {
		r.Enabled = v
	}

	r.Weight = 1
	if f, ok := toFloat(truthyOr(data["weight"], 1)); ok {
		w := int(f)
		if w < 1 {
			w = 1
		}
		if w > 100 {
			w = 100
		}
		r.Weight = w
	}
	if s, ok := data["note"].(string); ok {
		s = strings.TrimSpace(s)
		if rs := []rune(s); len(rs) > 200 {
			s = string(rs[:200])
		}
		r.Note = s
	}
	r.DisabledModels = realm.NormaliseDisabledModels(data["disabledModels"])

	if f, ok := toFloat(data["restrictedUntil"]); ok && f > 0 {
		r.RestrictedUntil = f
	}
	if m, ok := data["credits"].(map[string]any); ok && len(m) > 0 {
		r.Credits = m
	}
	r.LastCheckin = nilIfFalsy(data["lastCheckin"])
	r.LastDailyChat = nilIfFalsy(data["lastDailyChat"])
	r.LastActivityReport = nilIfFalsy(data["lastActivityReport"])

	if caps, ok := data["modelCaps"].(map[string]any); ok {
		for mid, entry := range caps {
			em, ok := entry.(map[string]any)
			if !ok || !realm.IsFreeCapModel(r.Realm, mid) {
				continue
			}
			at, ok1 := toFloat(truthyOr(em["at"], 0))
			reset, ok2 := toFloat(truthyOr(em["reset"], 0))
			if !ok1 || !ok2 || at <= 0 || reset <= 0 {
				continue
			}
			if r.ModelCaps == nil {
				r.ModelCaps = map[string]ModelCap{}
			}
			r.ModelCaps[mid] = ModelCap{At: at, Reset: reset}
		}
	}

	if cool, ok := data["modelCool"].(map[string]any); ok {
		for mid, v := range cool {
			until, ok := toFloat(truthyOr(v, 0))
			if !ok || until <= 0 || mid == "" {
				continue
			}
			if r.ModelCool == nil {
				r.ModelCool = map[string]float64{}
			}
			r.ModelCool[mid] = until
		}
	}

	// modelCoolWhy is purely informational: it explains why a model window is
	// running (soft 429 / upstream quota / entitlement). Absent in older files.
	if why, ok := data["modelCoolWhy"].(map[string]any); ok {
		for mid, v := range why {
			s, ok := v.(string)
			if !ok || mid == "" || s == "" {
				continue
			}
			if r.ModelCoolWhy == nil {
				r.ModelCoolWhy = map[string]string{}
			}
			r.ModelCoolWhy[mid] = s
		}
	}

	if v, ok := data["tasksEnabled"]; ok {
		r.TasksEnabled = v
	}
	if s, ok := data["tier"].(string); ok {
		r.Tier = strings.TrimSpace(s)
	}

	for k, v := range data {
		if !knownKeys[k] {
			r.Extra[k] = v
		}
	}
	return r
}

func truthyOr(v any, def any) any {
	if truthyAny(v) {
		return v
	}
	return def
}

func nilIfFalsy(v any) any {
	if truthyAny(v) {
		return v
	}
	return nil
}

// NormalizeProduct collapses the accepted spellings to workbuddy / vscode / cli.
func NormalizeProduct(v any) string {
	s := ""
	if str, ok := v.(string); ok {
		s = str
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "vscode", "vsc", "ide":
		return "vscode"
	case "cli", "codebuddy":
		return "cli"
	}
	return "workbuddy"
}

var unsafeUID = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// FileName returns the file name this record is stored under (<safe-uid>.json).
// ok is false when the uid sanitises to nothing and a random name is needed.
func (r *Record) FileName() (name string, ok bool) {
	safe := strings.Trim(unsafeUID.ReplaceAllString(r.UID, "_"), "_ ")
	if safe == "" {
		return "", false
	}
	return safe + ".json", true
}

func roundTripNumber(f float64) any {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return int64(f)
	}
	return f
}
