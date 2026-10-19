package realm

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// Claims decodes the (unverified) payload segment of a JWT. Anything that does
// not decode to a JSON object yields nil, mirroring the legacy `_jwt_claims`
// which returned {} on every failure. Like Python's lenient b64decode, bytes
// outside the URL-safe alphabet are discarded before decoding.
func Claims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	seg := make([]byte, 0, len(parts[1]))
	for i := 0; i < len(parts[1]); i++ {
		c := parts[1][i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
			seg = append(seg, c)
		}
	}
	raw, err := base64.RawURLEncoding.DecodeString(string(seg))
	if err != nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return nil
	}
	return out
}

// truthy mimics Python truthiness for the JSON value kinds that appear in claims.
func truthy(v any) bool {
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

// pyStr mimics Python's str() for scalar claim values.
func pyStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		if t {
			return "True"
		}
		return "False"
	}
	return ""
}

func claimStr(token, key string) string {
	v := Claims(token)[key]
	if !truthy(v) {
		return ""
	}
	return pyStr(v)
}

// JWTExp returns the `exp` claim as integer seconds, 0 when absent or invalid
// (Python: int(claims.get("exp") or 0) inside try/except).
func JWTExp(token string) int64 {
	v := Claims(token)["exp"]
	if !truthy(v) {
		return 0
	}
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		if f, err := t.Float64(); err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
			return int64(f)
		}
	case string:
		if i, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
			return i
		}
	case bool:
		if t {
			return 1
		}
	}
	return 0
}

// JWTUID returns the `sub` claim.
func JWTUID(token string) string { return claimStr(token, "sub") }

// JWTIssuer returns the `iss` claim.
func JWTIssuer(token string) string { return claimStr(token, "iss") }

// JWTEnterpriseID returns the `enterprise_id` claim.
func JWTEnterpriseID(token string) string { return claimStr(token, "enterprise_id") }

// NormalizeEpoch turns seconds-or-milliseconds into integer seconds. Values
// that are not numeric yield 0, as in the legacy normalize_epoch.
func NormalizeEpoch(value any) int64 {
	var f float64
	switch t := value.(type) {
	case nil:
		return 0
	case bool:
		if t {
			f = 1
		}
	case int:
		f = float64(t)
	case int64:
		f = float64(t)
	case float64:
		f = t
	case json.Number:
		x, err := t.Float64()
		if err != nil {
			return 0
		}
		f = x
	case string:
		x, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0
		}
		f = x
	default:
		return 0
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	if f > 1e11 {
		f /= 1000.0
	}
	return int64(f)
}
