package codearts

import (
	"strings"

	"wb2api/internal/store/accounts"
)

// extraMap returns the account's CodeArts credential map. Two layouts exist:
// nested under Extra["codearts"] (what the importer writes) and flat, with the
// keys directly in Extra (hand-written files). Both are read the same way.
func extraMap(rec *accounts.Record) map[string]any {
	if rec == nil || rec.Extra == nil {
		return nil
	}
	if v, ok := rec.Extra[ExtraKey]; ok {
		if m, ok := v.(map[string]any); ok {
			return m
		}
	}
	return rec.Extra
}

// Extra returns the account's CodeArts credential map. Callers that write to it
// must copy it first: accounts.Record values share the map.
func Extra(rec *accounts.Record) map[string]any { return extraMap(rec) }

func extraStr(m map[string]any, names ...string) string {
	for _, n := range names {
		if v, ok := m[n]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// CredentialOf assembles the signing credential of one account. The STS token
// lives in the record's AccessToken (so the pool's expiry/refresh logic sees
// it); the AK/SK pair lives in Extra.
func CredentialOf(rec *accounts.Record) Credential {
	if rec == nil {
		return Credential{}
	}
	m := extraMap(rec)
	token := strings.TrimSpace(rec.AccessToken)
	if token == "" {
		token = extraStr(m, "security_token", "securityToken", "cloud_dragon_token")
	}
	return Credential{
		AccessKeyID:     extraStr(m, "access_key_id", "accessKeyId", "access_key"),
		SecretAccessKey: extraStr(m, "secret_access_key", "secretAccessKey", "secret_key"),
		SecurityToken:   token,
		Expiration:      extraStr(m, "expiration", "expires_at"),
	}
}

// DPoPKeyOf reads the account's OAuth DPoP private key. A nil result means the
// credential predates DPoP and cannot be refreshed.
func DPoPKeyOf(rec *accounts.Record) DPoPKey {
	m := extraMap(rec)
	raw, ok := m["dpop_private_key"]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case map[string]any:
		out := make(DPoPKey, len(v))
		for k, val := range v {
			if s, ok := val.(string); ok {
				out[k] = s
			}
		}
		return out
	case map[string]string:
		out := make(DPoPKey, len(v))
		for k, val := range v {
			out[k] = val
		}
		return out
	}
	return nil
}

// CodeVerifierOf returns the PKCE verifier bound to the refresh token.
func CodeVerifierOf(rec *accounts.Record) string {
	return extraStr(extraMap(rec), "code_verifier", "codeVerifier")
}

// ApplyToken folds a refreshed token response into a copy of the record: the
// STS token, the rotated refresh token, the AK/SK pair and the expiry all move
// together, because the next signature uses the new pair.
func ApplyToken(rec *accounts.Record, t *TokenResponse) *accounts.Record {
	next := *rec
	next.AccessToken = t.Credentials.SecurityToken
	if t.RefreshToken != "" {
		next.RefreshToken = t.RefreshToken
	}
	if exp := t.Expiry(); !exp.IsZero() {
		next.ExpiresAt = exp.Unix()
	}
	m := map[string]any{}
	for k, v := range extraMap(rec) {
		m[k] = v
	}
	m["access_key_id"] = t.Credentials.AccessKeyID
	m["secret_access_key"] = t.Credentials.SecretAccessKey
	m["security_token"] = t.Credentials.SecurityToken
	if t.Credentials.Expiration != "" {
		m["expiration"] = t.Credentials.Expiration
	}
	if next.Extra == nil {
		next.Extra = map[string]any{}
	}
	// Copy the map before writing: next shares the record's header, and
	// mutating the nested map in place would edit the live account too.
	extra := make(map[string]any, len(next.Extra)+1)
	for k, v := range next.Extra {
		extra[k] = v
	}
	extra[ExtraKey] = m
	next.Extra = extra
	return &next
}

// IsBenefitModel reports whether the account classifies model as a limited-time
// benefit model (free plan). Such a model needs the maas_type: benefit header;
// the answer is per account because plans differ inside one pool.
func IsBenefitModel(rec *accounts.Record, model string) bool {
	if rec == nil || model == "" {
		return false
	}
	m := extraMap(rec)
	for _, key := range []string{"benefit_models", "benefitModels"} {
		switch v := m[key].(type) {
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok && strings.EqualFold(strings.TrimSpace(s), model) {
					return true
				}
			}
		case []string:
			for _, s := range v {
				if strings.EqualFold(strings.TrimSpace(s), model) {
					return true
				}
			}
		case string:
			for _, s := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
				if strings.EqualFold(strings.TrimSpace(s), model) {
					return true
				}
			}
		}
	}
	return false
}

// ModelsOf returns the account's advertised model ids (Extra["models"]).
func ModelsOf(rec *accounts.Record) []string {
	if rec == nil {
		return nil
	}
	m := extraMap(rec)
	out := []string{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		for _, existing := range out {
			if existing == s {
				return
			}
		}
		out = append(out, s)
	}
	switch v := m["models"].(type) {
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				add(s)
			}
		}
	case []string:
		for _, s := range v {
			add(s)
		}
	case string:
		for _, s := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ';' || r == '\n' || r == ' ' }) {
			add(s)
		}
	}
	return out
}

// WireName maps a requested model to the spelling the chat endpoint accepts.
//
// The catalogue keeps both spellings: ID (what the API takes) and Name (what the
// panel shows). They differ — "deepseek-v4.1-flash" is callable while
// "DeepSeek-V4.1-Flash" is refused with TM.000010051 — so a request that names
// the display form is translated instead of forwarded.
func WireName(rec *accounts.Record, model string) string {
	if rec == nil || model == "" {
		return model
	}
	extra := Extra(rec)
	if extra == nil {
		return model
	}
	known := map[string]bool{}
	if list, ok := extra["models"].([]any); ok {
		for _, v := range list {
			if id, ok := v.(string); ok {
				if id == model {
					return model // already the wire spelling
				}
				known[strings.ToLower(id)] = true
			}
		}
	}
	if names, ok := extra["model_names"].(map[string]any); ok {
		for id, raw := range names {
			name, _ := raw.(string)
			if strings.EqualFold(name, model) {
				return id
			}
		}
	}
	return model
}
