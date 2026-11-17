package codearts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrRefreshExpired means the refresh token is terminal: the account must be
// signed in again. It is deliberately distinct from a transport failure so the
// refresher backs off for a day instead of retrying every minute.
var ErrRefreshExpired = errors.New("codearts: refresh token expired or rejected")

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

// TokenURL is the STS token endpoint for a snap-manager base.
func TokenURL(base string) string { return strings.TrimRight(base, "/") + TokenPath }

// Refresh exchanges a refresh token for fresh STS credentials.
//
// codeVerifier and jwk belong to the credential that was signed in: STS binds
// the refresh token to both, and a mismatch surfaces as
// "invalid refresh token: InvalidDPoPHeader".
func Refresh(ctx context.Context, hc *http.Client, base, refreshToken, codeVerifier string, jwk DPoPKey) (*TokenResponse, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, ErrRefreshExpired
	}
	if _, err := SignProof(jwk, TokenURL(base), time.Now()); err != nil {
		// A credential without its DPoP key can still be used until the STS
		// token expires, but it can never be refreshed.
		return nil, fmt.Errorf("%w (dpop: %v)", ErrRefreshExpired, err)
	}
	form := url.Values{}
	form.Set("client_id", ClientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	if codeVerifier != "" {
		form.Set("code_verifier", codeVerifier)
	}
	return requestToken(ctx, hc, base, form, jwk, "refresh")
}

// requestToken posts an OAuth2 form with a fresh DPoP proof and normalises the
// token response. kind only labels errors ("refresh", "authorization_code").
func requestToken(ctx context.Context, hc *http.Client, base string, form url.Values, jwk DPoPKey, kind string) (*TokenResponse, error) {
	target := TokenURL(base)
	proof, err := SignProof(jwk, target, time.Now())
	if err != nil {
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
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codearts %s: %w", kind, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		var tr TokenResponse
		_ = json.Unmarshal(raw, &tr)
		msg := tr.Error + " " + tr.ErrorCode + " " + tr.ErrorMessage
		lower := strings.ToLower(msg)
		if kind == "refresh" && (tr.Error == "invalid_grant" || strings.Contains(msg, "ExpiredRefreshToken") ||
			strings.Contains(msg, "InvalidDPoPHeader") || strings.Contains(lower, "invalid client id") ||
			strings.Contains(lower, "has been used")) {
			return nil, fmt.Errorf("%w (http=%d code=%s msg=%s)", ErrRefreshExpired, resp.StatusCode, tr.ErrorCode, truncate(tr.ErrorMessage, 160))
		}
		return nil, fmt.Errorf("codearts %s HTTP %d: %s", kind, resp.StatusCode, truncate(string(raw), 200))
	}
	var out TokenResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("codearts %s: parse response: %w", kind, err)
	}
	if out.Credentials.SecurityToken == "" {
		if out.Credential.SecurityToken != "" {
			out.Credentials = Credential{
				AccessKeyID:     out.Credential.Access,
				SecretAccessKey: out.Credential.Secret,
				SecurityToken:   out.Credential.SecurityToken,
				Expiration:      out.Credential.ExpiresAt,
			}
			return &out, nil
		}
		return nil, fmt.Errorf("codearts %s: response carries no credentials", kind)
	}
	return &out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
