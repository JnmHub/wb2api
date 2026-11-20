package accounts

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"strings"
	"time"

	"wb2api/internal/realm"
)

// RaccoonSafetyWindow is the minimum lead for a refreshable access token.
// A throttled refresh leaves a valid token usable until its actual expiry.
const RaccoonSafetyWindow = 2 * time.Minute

const raccoonAuthKey = "raccoonAuth"

// RaccoonAuth records why an account needs a new sign-in. Raccoon's IAM
// credentials have no refresh grant, so "rejected and not refreshable" is a
// terminal state until the operator re-imports a token; without this flag the
// pool would keep dispatching turns that cannot succeed.
type RaccoonAuth struct {
	Version        string
	Reason         string
	Login          bool
	AccessRejected bool
	Failures       int
	RetryAt        time.Time
}

// RaccoonFields returns the stored raccoon metadata. Two layouts exist: nested
// under Extra["raccoon"] (what the importer writes) and flat, with the keys
// directly in Extra (hand-written files). Both are read the same way.
func (r *Record) RaccoonFields() map[string]any {
	if r == nil {
		return nil
	}
	if nested, ok := r.Extra["raccoon"].(map[string]any); ok {
		return nested
	}
	return r.Extra
}

// RaccoonCredentialVersion identifies the credential generation. Scheduling
// state stores only the digest, never the token.
func (r *Record) RaccoonCredentialVersion() string {
	access, refresh := r.AccessToken, r.RefreshToken
	if access == "" {
		access = RaccoonStringOf(r.RaccoonFields(), "accessToken", "access_token", "token")
	}
	if refresh == "" {
		refresh = RaccoonStringOf(r.RaccoonFields(), "refreshToken", "refresh_token")
	}
	sum := sha256.Sum256([]byte(r.Realm + "|" + access + "|" + refresh))
	return hex.EncodeToString(sum[:])
}

func (r *Record) MatchesRaccoonCredential(observed *Record) bool {
	return r != nil && observed != nil && r.UID == observed.UID &&
		realm.IsRaccoon(r.Realm) && r.Realm == observed.Realm && r.ExpiresAt == observed.ExpiresAt &&
		r.RaccoonCredentialVersion() == observed.RaccoonCredentialVersion() && r.RaccoonOrgCode() == observed.RaccoonOrgCode()
}

// WithRaccoonFields changes only the metadata namespace and preserves the
// existing nested or flat account layout.
func (r *Record) WithRaccoonFields(change func(map[string]any)) *Record {
	next := *r
	next.Extra = maps.Clone(r.Extra)
	if next.Extra == nil {
		next.Extra = make(map[string]any)
	}
	if nested, ok := r.Extra["raccoon"].(map[string]any); ok {
		fields := maps.Clone(nested)
		if fields == nil {
			fields = make(map[string]any)
		}
		change(fields)
		next.Extra["raccoon"] = fields
	} else {
		change(next.Extra)
	}
	return &next
}

// RaccoonOrgCode is the X-Org-Code value: the office identity when the account
// belongs to an organisation, empty for a personal account ("personal" is the
// upstream's spelling for "no organisation").
func (r *Record) RaccoonOrgCode() string {
	v := RaccoonStringOf(r.RaccoonFields(), "officeIdentity", "office_identity")
	if v == "" || strings.EqualFold(v, "personal") {
		return ""
	}
	return v
}

// RaccoonStringOf reads the first non-empty string among names.
func RaccoonStringOf(m map[string]any, names ...string) string {
	for _, name := range names {
		if v, ok := m[name].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// RaccoonAuthState reads the persisted rejection state for this credential
// generation; a new token supersedes it.
func (r *Record) RaccoonAuthState() RaccoonAuth {
	if r == nil || !realm.IsRaccoon(r.Realm) {
		return RaccoonAuth{}
	}
	m, _ := r.Extra[raccoonAuthKey].(map[string]any)
	version, _ := m["version"].(string)
	if version == "" || version != r.RaccoonCredentialVersion() {
		return RaccoonAuth{}
	}
	reason, _ := m["reason"].(string)
	login, _ := m["login"].(bool)
	rejected, _ := m["accessRejected"].(bool)
	failures, _ := toFloat(m["failures"])
	retry, _ := toFloat(m["retryAt"])
	state := RaccoonAuth{Version: version, Reason: reason, Login: login, AccessRejected: rejected, Failures: int(failures)}
	if retry > 0 {
		state.RetryAt = time.UnixMilli(int64(retry))
	}
	return state
}

// WithRaccoonAuth updates the outcome without mutating a published record.
func (r *Record) WithRaccoonAuth(state RaccoonAuth) *Record {
	next := *r
	next.Extra = make(map[string]any, len(r.Extra)+1)
	for k, v := range r.Extra {
		next.Extra[k] = v
	}
	if state.Reason != "" {
		var retry int64
		if !state.RetryAt.IsZero() {
			retry = state.RetryAt.UnixMilli()
		}
		next.Extra[raccoonAuthKey] = map[string]any{
			"version": r.RaccoonCredentialVersion(), "reason": state.Reason,
			"login": state.Login, "accessRejected": state.AccessRejected, "failures": state.Failures, "retryAt": retry,
		}
	} else {
		delete(next.Extra, raccoonAuthKey)
	}
	return &next
}

// RaccoonNeedsLogin excludes a rejected credential from serving.
func (r *Record) RaccoonNeedsLogin() bool { return r.RaccoonAuthState().Login }

// RaccoonAccessUsable reports whether the stored token can be sent right now.
// An unknown expiry (0) counts as usable: the upstream answers 401 and the
// refresh path takes over, which beats refusing to try.
func (r *Record) RaccoonAccessUsable(now time.Time) bool {
	if r == nil || r.AccessToken == "" {
		return false
	}
	auth := r.RaccoonAuthState()
	if auth.Login || auth.AccessRejected {
		return false
	}
	if r.ExpiresAt == 0 {
		return true
	}
	return r.ExpiresAt > now.Unix()
}
