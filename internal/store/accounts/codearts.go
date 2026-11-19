package accounts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"wb2api/internal/realm"
)

// CodeArtsSafetyWindow leaves time to complete signed calls before STS expiry.
const CodeArtsSafetyWindow = 2 * time.Minute

const codeArtsAuthKey = "codeartsAuth"

// CodeArtsAuth records an outcome for a particular OAuth credential version.
// A new sign-in or a credential replacement supersedes this state.
type CodeArtsAuth struct {
	Version        string
	Reason         string
	Failures       int
	RetryAt        time.Time
	Login          bool
	AccessRejected bool
}

// CodeArtsFields returns the stored signing and refresh metadata.
func (r *Record) CodeArtsFields() map[string]any {
	if r == nil {
		return nil
	}
	if nested, ok := r.Extra["codearts"].(map[string]any); ok {
		return nested
	}
	return r.Extra
}

func credentialField(m map[string]any, names ...string) string {
	for _, name := range names {
		if v, ok := m[name].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// CodeArtsCredentialVersion identifies the refresh token and its binding.
// Scheduling state stores only the digest.
func (r *Record) CodeArtsCredentialVersion() string {
	fields := r.CodeArtsFields()
	raw, _ := json.Marshal([]any{
		r.RefreshToken,
		credentialField(fields, "client_id", "clientId"),
		credentialField(fields, "code_verifier", "codeVerifier"),
		fields["dpop_private_key"],
	})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// CodeArtsAccessVersion identifies the STS token without keeping a second copy.
func (r *Record) CodeArtsAccessVersion() string {
	sum := sha256.Sum256([]byte(r.AccessToken))
	return hex.EncodeToString(sum[:])
}

func (r *Record) CodeArtsAuthState() CodeArtsAuth {
	if r == nil || !realm.IsCodeArts(r.Realm) {
		return CodeArtsAuth{}
	}
	m, _ := r.Extra[codeArtsAuthKey].(map[string]any)
	v, _ := m["version"].(string)
	if v == "" || v != r.CodeArtsCredentialVersion() {
		return CodeArtsAuth{}
	}
	reason, _ := m["reason"].(string)
	login, _ := m["login"].(bool)
	failures, _ := toFloat(m["failures"])
	retry, _ := toFloat(m["retryAt"])
	rejected, _ := m["rejectedAccess"].(string)
	state := CodeArtsAuth{Version: v, Reason: reason, Login: login, Failures: int(failures)}
	state.AccessRejected = rejected != "" && rejected == r.CodeArtsAccessVersion()
	if retry > 0 {
		state.RetryAt = time.UnixMilli(int64(retry))
	}
	return state
}

// WithCodeArtsAuth updates the outcome without mutating a published record.
func (r *Record) WithCodeArtsAuth(state CodeArtsAuth) *Record {
	next := *r
	next.Extra = make(map[string]any, len(r.Extra)+1)
	for k, v := range r.Extra {
		next.Extra[k] = v
	}
	if state.Reason != "" {
		var retry int64
		rejected := ""
		if state.AccessRejected {
			rejected = r.CodeArtsAccessVersion()
		}
		if !state.RetryAt.IsZero() {
			retry = state.RetryAt.UnixMilli()
		}
		next.Extra[codeArtsAuthKey] = map[string]any{
			"version": r.CodeArtsCredentialVersion(), "reason": state.Reason,
			"login": state.Login, "failures": state.Failures, "retryAt": retry, "rejectedAccess": rejected,
		}
	} else {
		delete(next.Extra, codeArtsAuthKey)
	}
	return &next
}

// CodeArtsNeedsLogin excludes a rejected OAuth generation from serving.
func (r *Record) CodeArtsNeedsLogin() bool {
	return r.CodeArtsAuthState().Login
}

// CodeArtsAccessUsable requires a complete, sufficiently fresh STS credential.
func (r *Record) CodeArtsAccessUsable(now time.Time) bool {
	if r == nil || r.AccessToken == "" || r.ExpiresAt <= now.Add(CodeArtsSafetyWindow).Unix() {
		return false
	}
	m := r.CodeArtsFields()
	auth := r.CodeArtsAuthState()
	return !auth.Login && !auth.AccessRejected &&
		credentialField(m, "access_key_id", "accessKeyId", "access_key") != "" &&
		credentialField(m, "secret_access_key", "secretAccessKey", "secret_key") != ""
}
