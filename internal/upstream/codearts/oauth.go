package codearts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ErrRefreshExpired means the refresh token is terminal: the account must be
// signed in again.
var ErrRefreshExpired = errors.New("codearts: refresh token expired or rejected")

// RefreshFailure is a sanitized STS failure. Response bodies may contain secrets.
type RefreshFailure struct {
	Reason     string
	HTTPStatus int
	RetryAt    time.Time
	NeedsLogin bool
}

func (e *RefreshFailure) Error() string {
	if e.HTTPStatus != 0 {
		return fmt.Sprintf("codearts STS: %s (HTTP %d)", e.Reason, e.HTTPStatus)
	}
	return "codearts STS: " + e.Reason
}

func (e *RefreshFailure) Is(target error) bool {
	return target == ErrRefreshExpired && e.NeedsLogin
}

// TokenResponse is the snap-manager oauth2/tokens payload.
type TokenResponse struct {
	UserID       string     `json:"user_id"`
	UserName     string     `json:"user_name"`
	RefreshToken string     `json:"refresh_token"`
	Credentials  Credential `json:"credentials"`
	// Legacy ticket-channel fields.
	Credential struct {
		Access        string `json:"access"`
		Secret        string `json:"secret"`
		SecurityToken string `json:"securitytoken"`
		ExpiresAt     string `json:"expires_at"`
	} `json:"credential"`

	Error        string `json:"error"`
	ErrorCode    string `json:"error_code"`
	ErrorMessage string `json:"error_msg"`
}

// Expiry parses the credential expiration; the zero time means "unknown".
func (t *TokenResponse) Expiry() time.Time {
	raw := strings.TrimSpace(t.Credentials.Expiration)
	if raw == "" {
		raw = strings.TrimSpace(t.Credential.ExpiresAt)
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z0700"} {
		if ts, err := time.Parse(layout, raw); err == nil {
			return ts.UTC()
		}
	}
	return time.Time{}
}

// TokenURL is the token endpoint for an STS base (see STSHost).
func TokenURL(stsBase string) string { return strings.TrimRight(stsBase, "/") + TokenPath }

// Refresh exchanges a refresh token for fresh STS credentials. stsBase is the
// STS host (STSHost), not snap-access.
//
// codeVerifier and jwk belong to the credential that was signed in: STS binds
// the refresh token to both, and a mismatch surfaces as
// "invalid refresh token: InvalidDPoPHeader".
func Refresh(ctx context.Context, hc *http.Client, stsBase, refreshToken, codeVerifier string, jwk DPoPKey) (*TokenResponse, error) {
	return RefreshWithClientID(ctx, hc, stsBase, ClientID, refreshToken, codeVerifier, jwk)
}

// RefreshWithClientID keeps the original OAuth client binding when migrating
// credentials. Replaying a rotating refresh token with another ID is unsafe.
func RefreshWithClientID(ctx context.Context, hc *http.Client, stsBase, clientID, refreshToken, codeVerifier string, jwk DPoPKey) (*TokenResponse, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, &RefreshFailure{Reason: "missing_refresh_token", NeedsLogin: true}
	}
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	if codeVerifier != "" {
		form.Set("code_verifier", codeVerifier)
	}
	return requestToken(ctx, hc, stsBase, form, jwk, "refresh")
}

// requestToken posts an OAuth2 form with a fresh DPoP proof and normalises the
// token response. kind only labels errors ("refresh", "authorization_code").
func requestToken(ctx context.Context, hc *http.Client, stsBase string, form url.Values, jwk DPoPKey, kind string) (*TokenResponse, error) {
	target := TokenURL(stsBase)
	proof, err := SignProof(jwk, target, time.Now())
	if err != nil {
		if kind == "refresh" {
			return nil, &RefreshFailure{Reason: "invalid_dpop_key", NeedsLogin: true}
		}
		return nil, fmt.Errorf("codearts %s: dpop proof: %w", kind, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("DPoP", proof)
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	// Force a single exchange. Following a redirect could forward both the
	// bound token and its proof to an unintended peer.
	client := *hc
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	var wrote atomic.Bool
	if kind == "refresh" {
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			WroteRequest: func(info httptrace.WroteRequestInfo) { wrote.Store(info.Err == nil) },
		}))
	}
	resp, err := client.Do(req)
	if err != nil {
		if kind == "refresh" {
			if wrote.Load() {
				return nil, &RefreshFailure{Reason: "refresh_outcome_unknown", NeedsLogin: true}
			}
			return nil, &RefreshFailure{Reason: "refresh_connection_failed"}
		}
		return nil, fmt.Errorf("codearts %s: %w", kind, err)
	}
	defer resp.Body.Close()
	if kind == "refresh" && resp.StatusCode == http.StatusTooManyRequests {
		return nil, &RefreshFailure{
			Reason: "sts_rate_limited", HTTPStatus: resp.StatusCode,
			RetryAt: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		}
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var out TokenResponse
	decodeErr := json.Unmarshal(raw, &out)
	if kind == "refresh" {
		if resp.StatusCode/100 == 4 || resp.StatusCode/100 == 2 {
			if reason := terminalRefreshReason(out); reason != "" {
				return nil, &RefreshFailure{Reason: reason, HTTPStatus: resp.StatusCode, NeedsLogin: true}
			}
		}
		if resp.StatusCode/100 != 2 {
			return nil, &RefreshFailure{Reason: "refresh_upstream_error", HTTPStatus: resp.StatusCode}
		}
		if readErr != nil || decodeErr != nil {
			return nil, &RefreshFailure{Reason: "refresh_outcome_unknown", HTTPStatus: resp.StatusCode, NeedsLogin: true}
		}
	} else if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("codearts %s HTTP %d: %s", kind, resp.StatusCode, truncate(string(raw), 200))
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("codearts %s: parse response: %w", kind, decodeErr)
	}
	if out.Credentials.SecurityToken == "" {
		if out.Credential.SecurityToken != "" {
			out.Credentials = Credential{
				AccessKeyID:     out.Credential.Access,
				SecretAccessKey: out.Credential.Secret,
				SecurityToken:   out.Credential.SecurityToken,
				Expiration:      out.Credential.ExpiresAt,
			}
		} else if kind == "refresh" {
			return nil, &RefreshFailure{Reason: "refresh_outcome_unknown", HTTPStatus: resp.StatusCode, NeedsLogin: true}
		} else {
			return nil, fmt.Errorf("codearts %s: response carries no credentials", kind)
		}
	}
	return &out, nil
}

func terminalRefreshReason(out TokenResponse) string {
	msg := strings.ToLower(out.Error + " " + out.ErrorCode + " " + out.ErrorMessage)
	switch {
	case strings.Contains(msg, "has been used"), strings.Contains(msg, "already used"):
		return "refresh_token_used"
	case strings.Contains(msg, "expiredrefreshtoken"), strings.Contains(msg, "refresh token has expired"),
		strings.Contains(msg, "refresh token is expired"):
		return "refresh_token_expired"
	case strings.Contains(msg, "invaliddpopheader"), strings.Contains(msg, "invalid client id"),
		strings.Contains(msg, "invalid_client"), strings.Contains(msg, "invalidcodeverifier"),
		strings.Contains(msg, "invalid code verifier"):
		return "refresh_binding_rejected"
	case strings.Contains(msg, "revoked"), strings.Contains(msg, "invalid_grant"),
		strings.Contains(msg, "invalid refresh token"):
		return "refresh_token_rejected"
	default:
		return ""
	}
}

func parseRetryAfter(v string, now time.Time) time.Time {
	v = strings.TrimSpace(v)
	if n, err := strconv.ParseInt(v, 10, 32); err == nil && n >= 0 {
		return now.Add(time.Duration(n) * time.Second)
	}
	if date, err := http.ParseTime(v); err == nil && date.After(now) {
		return date
	}
	return time.Time{}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// IsExpiredCredentialError recognizes an explicit STS-expiry response surfaced
// by a signed operation.
func IsExpiredCredentialError(err error) bool {
	return err != nil && IsExpiredAccessToken([]byte(err.Error()))
}

// IsExpiredAccessToken distinguishes STS expiry from other 400/401 responses.
func IsExpiredAccessToken(body []byte) bool {
	s := strings.ToLower(string(body))
	return strings.Contains(s, "security token has expired") ||
		strings.Contains(s, "security token is expired") ||
		strings.Contains(s, "expiredsecuritytoken") ||
		(strings.Contains(s, "apig.0602") && strings.Contains(s, "expired"))
}
