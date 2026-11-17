package codearts

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
	"time"

	"wb2api/internal/upstream/identity"
)

// SignAlgorithm is the Huawei Cloud signing algorithm name.
const SignAlgorithm = "SDK-HMAC-SHA256"

// Credential is the AK/SK triple of one account.
type Credential struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SecurityToken   string `json:"security_token"`
	// Expiration is the STS expiry as reported by snap-manager (RFC3339). It
	// is informational here: accounts.Record.ExpiresAt is what the refresher
	// schedules from.
	Expiration string `json:"expiration,omitempty"`
}

// Signer produces the signing headers of one request. It exists so tests can
// pin the clock.
type Signer struct {
	// Now overrides the clock (tests). A nil clock is time.Now.
	Now func() time.Time
}

// SignHeaders adds X-Sdk-Date / X-Security-Token / X-Sdk-Content-Sha256 and the
// Authorization header to h and returns it. The signature covers every header
// present at signing time, so nothing may be added afterwards except headers
// the upstream does not verify (it only checks the SignedHeaders list).
func (s Signer) SignHeaders(h identity.Headers, method, rawURL string, body []byte, cred Credential) identity.Headers {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		// Callers build the URL from realm constants; a malformed one is a
		// programming error, and signing an empty host would produce a
		// signature the upstream rejects with an opaque 403.
		return h
	}
	date := now().UTC().Format("20060102T150405Z")
	payloadHash := sha256Hex(body)

	out := identity.Headers{}
	out = append(out, h...)
	setHeader(&out, "Host", u.Host)
	setHeader(&out, "X-Sdk-Date", date)
	if cred.SecurityToken != "" {
		setHeader(&out, "X-Security-Token", cred.SecurityToken)
	}
	setHeader(&out, "X-Sdk-Content-Sha256", payloadHash)

	canonicalHeaderBlock, signedHeaders := canonicalHeaders(out)
	canonicalRequest := strings.Join([]string{
		method,
		canonicalURI(u.EscapedPath()),
		canonicalQuery(u.RawQuery),
		canonicalHeaderBlock,
		signedHeaders,
		payloadHash,
	}, "\n")
	stringToSign := strings.Join([]string{
		SignAlgorithm,
		date,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")
	signature := hmacHex(cred.SecretAccessKey, stringToSign)
	out = append(out, identity.P("Authorization", SignAlgorithm+
		" Access="+cred.AccessKeyID+
		", SignedHeaders="+signedHeaders+
		", Signature="+signature))
	return out
}

// setHeader replaces an existing pair in place (keeping insertion order) or
// appends it.
func setHeader(h *identity.Headers, name, value string) {
	for i := range *h {
		if strings.EqualFold((*h)[i].Name, name) {
			(*h)[i].Value = value
			return
		}
	}
	*h = append(*h, identity.P(name, value))
}

// canonicalHeaders renders the canonical header block and the SignedHeaders
// list: every header, lower-cased, sorted by name.
func canonicalHeaders(h identity.Headers) (block, signed string) {
	idx := make(map[string]string, len(h))
	for _, p := range h {
		name := strings.ToLower(strings.TrimSpace(p.Name))
		if name == "" {
			continue
		}
		idx[name] = strings.TrimSpace(p.Value)
	}
	keys := make([]string, 0, len(idx))
	for k := range idx {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(k)
		sb.WriteByte(':')
		sb.WriteString(idx[k])
		sb.WriteByte('\n')
	}
	return sb.String(), strings.Join(keys, ";")
}

// canonicalURI percent-encodes every path segment and always ends with "/",
// exactly like the official SDK (the trailing slash is part of the contract).
func canonicalURI(path string) string {
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	out := strings.Join(segments, "/")
	if !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return out
}

// canonicalQuery sorts the query parameters by name and value.
func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return raw
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		vs := vals[k]
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacHex(key, msg string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}
