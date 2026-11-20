package openai

import (
	"net/url"
	"strings"

	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream/codearts"
	"wb2api/internal/upstream/raccoon"
)

// redactSecrets removes the account's credentials from a piece of upstream
// text before it can reach a client or a log.
//
// The vendors occasionally echo the rejected request back: a bad signature
// answer can quote the AK, an expired-token answer can quote the STS value.
// One echoed line is enough to hand out a working credential to everyone who
// can read the error, so every secret is masked in three spellings (raw,
// URL-escaped and JSON-escaped).
func redactSecrets(text string, rec *accounts.Record) string {
	if text == "" || rec == nil {
		return text
	}
	for _, secret := range accountSecrets(rec) {
		if len(secret) < 8 {
			continue
		}
		for _, form := range []string{secret, url.QueryEscape(secret), jsonEscape(secret)} {
			if form == "" {
				continue
			}
			text = strings.ReplaceAll(text, form, "[redacted]")
		}
	}
	return text
}

// accountSecrets lists the values that must never be echoed.
func accountSecrets(rec *accounts.Record) []string {
	out := []string{rec.AccessToken, rec.RefreshToken}
	if realm.IsCodeArts(rec.Realm) {
		extra := codearts.Extra(rec)
		for _, key := range []string{"access_key_id", "secret_access_key", "security_token", "dpop_private_key", "code_verifier", "client_id"} {
			if v, ok := extra[key].(string); ok {
				out = append(out, v)
			}
		}
	}
	if realm.IsRaccoon(rec.Realm) {
		for _, key := range []string{"accessToken", "access_token", "token", "refreshToken", "refresh_token"} {
			if v, ok := rec.RaccoonFields()[key].(string); ok {
				out = append(out, raccoon.StripBearer(v))
			}
		}
	}
	return out
}

// jsonEscape mirrors what encoding/json does to a string body, which is how an
// echoed secret comes back inside a JSON error document.
func jsonEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// redactBody applies redactSecrets to a raw upstream body.
func redactBody(body []byte, rec *accounts.Record) []byte {
	if len(body) == 0 || rec == nil {
		return body
	}
	return []byte(redactSecrets(string(body), rec))
}
