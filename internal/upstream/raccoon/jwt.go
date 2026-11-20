package raccoon

import (
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// StripBearer removes a leading "Bearer " (any case) and trims whitespace.
func StripBearer(token string) string {
	t := strings.TrimSpace(token)
	if len(t) > 7 && strings.EqualFold(t[:7], "bearer ") {
		return strings.TrimSpace(t[7:])
	}
	return t
}

// Claims decodes the payload segment of a JWT. The signature is NOT verified:
// the gateway never makes an authorization decision from a token, it only needs
// exp (refresh timing), name (default label) and the account id. A malformed
// token yields nil instead of an error so one hand-edited file cannot break a
// listing page.
func Claims(token string) map[string]any {
	raw := StripBearer(token)
	parts := strings.Split(raw, ".")
	if len(parts) < 2 || parts[1] == "" {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil
		}
	}
	v := map[string]any{}
	if err := json.Unmarshal(payload, &v); err != nil {
		return nil
	}
	return v
}

// Issuer returns the "iss" claim.
func Issuer(token string) string {
	s, _ := Claims(token)["iss"].(string)
	return strings.TrimSpace(s)
}

// IsIAM reports whether the token comes from the IAM identity service. IAM
// tokens have no refresh grant: once expired the account must sign in again.
func IsIAM(token string) bool {
	return strings.HasSuffix(strings.ToLower(Issuer(token)), "/iam")
}

// ExpectedIAMIssuer resolves the issuer an IAM token must carry
// (WB_RACCOON_IAM_ISSUER overrides the derivation from AuthBase).
func ExpectedIAMIssuer() string {
	if v := strings.TrimSpace(getenv("WB_RACCOON_IAM_ISSUER")); v != "" {
		return v
	}
	base := AuthBase()
	if i := strings.Index(base, "://"); i >= 0 {
		base = base[i+3:]
	}
	if i := strings.IndexByte(base, '/'); i >= 0 {
		base = base[:i]
	}
	if base == "" {
		base = "xiaohuanxiong.com"
	}
	return base + "/iam"
}

// Expiry returns the token's exp as a wall clock (zero when absent/invalid).
func Expiry(token string) time.Time {
	claims := Claims(token)
	if claims == nil {
		return time.Time{}
	}
	exp, ok := numberValue(claims["exp"])
	if !ok || exp <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(exp), 0)
}

// Expired reports whether exp has already passed (an absent exp is treated as
// "not expired": refusing to try would need a reason the token does not give).
func Expired(token string, now time.Time) bool {
	exp := Expiry(token)
	return !exp.IsZero() && !exp.After(now)
}

// ValidateIAM checks the IAM claims an account registration depends on: issuer,
// a non-empty subject and a future exp. The signature is still the upstream's
// business -- a forged token simply fails the first real call.
func ValidateIAM(token string) error {
	if !IsIAM(token) {
		return ErrNeedsLogin
	}
	claims := Claims(token)
	iss, _ := claims["iss"].(string)
	sub, _ := claims["sub"].(string)
	if strings.TrimSpace(iss) != ExpectedIAMIssuer() ||
		strings.TrimSpace(sub) == "" ||
		Expired(token, time.Now()) {
		return ErrNeedsLogin
	}
	return nil
}

// UserID extracts the platform account id:
//
//	IAM        -> sub
//	legacy JWT -> sid (hex) or iss (hex), converted to decimal
//	fallback   -> sub / id / user_id as plain text
//
// The hex step is not cosmetic: a legacy token's iss is e.g. "6d446a", and
// "7160938" is the id the account files (and the panel) are keyed by.
func UserID(token string) string {
	claims := Claims(token)
	if claims == nil {
		return ""
	}
	if IsIAM(token) {
		if sub, _ := claims["sub"].(string); strings.TrimSpace(sub) != "" {
			return strings.TrimSpace(sub)
		}
		return ""
	}
	for _, key := range []string{"sid", "iss"} {
		if s, _ := claims[key].(string); s != "" {
			if dec, ok := hexToDecimal(s); ok {
				return dec
			}
		}
	}
	for _, key := range []string{"sub", "id", "user_id"} {
		switch v := claims[key].(type) {
		case string:
			if t := strings.TrimSpace(v); t != "" {
				return t
			}
		case json.Number:
			return v.String()
		case float64:
			return strconv.FormatInt(int64(v), 10)
		}
	}
	return ""
}

// DisplayName is the token's display name (used as the default label).
func DisplayName(token string) string {
	claims := Claims(token)
	if claims == nil {
		return ""
	}
	for _, key := range []string{"name", "nickname", "preferred_username"} {
		if s, _ := claims[key].(string); strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// hexToDecimal converts a hex identifier to its decimal spelling. Non-hex
// input returns false: real tokens carry values like "web65aa393e65a6b6d446a51dde22c"
// in sid, which is not hex on purpose.
func hexToDecimal(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if s == "" {
		return "", false
	}
	n, ok := new(big.Int).SetString(s, 16)
	if !ok || n.Sign() <= 0 {
		return "", false
	}
	return n.String(), true
}

// numberValue reads a JSON number in any of the shapes encoding/json produces.
func numberValue(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case float64:
		return t, true
	case int64:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}
