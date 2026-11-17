package codearts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"wb2api/internal/realm"
)

// Login endpoints and plugin identity. The portal only accepts the real plugin
// pair ("snap_AIIDE" / its version) on the authorize URL, so they are constants
// rather than settings.
const (
	PortalHost    = "https://codearts.huaweicloud.com/portal"
	LoginTicket   = "/v1/login/ticket"
	PluginName    = "snap_AIIDE"
	PluginVersion = "5.2.0"
	// CallbackPath is where the portal redirects a desktop login.
	CallbackPath = "/oauth/callback"
)

// PKCE returns a verifier and its S256 challenge for one login attempt.
func PKCE() (verifier, challenge string, err error) {
	b := make([]byte, 64)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// RandomHex returns n random bytes as hex (ticket ids and secrets).
func RandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// AuthorizeURL builds the portal login link. port is the loopback callback port
// the desktop client would use; a server-side gateway passes 0 and relies on
// ticket polling instead.
func AuthorizeURL(ticketID, challenge string, port int) string {
	q := url.Values{}
	q.Set("theme", "2")
	q.Set("locale", "zh-cn")
	q.Set("uri_scheme", ClientID)
	q.Set("client_id", ClientID)
	q.Set("port", fmt.Sprint(port))
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "SHA-256")
	q.Set("ticket_id", ticketID)
	q.Set("plugin-name", PluginName)
	q.Set("plugin-version", PluginVersion)
	return strings.TrimRight(PortalHost, "/") + "/authorize?" + q.Encode()
}

// ExchangeCode trades an authorization code for STS credentials (the desktop
// callback path). jwk must be persisted with the refresh token.
func ExchangeCode(ctx context.Context, hc *http.Client, stsBase, code, verifier string, port int, jwk DPoPKey) (*TokenResponse, error) {
	form := url.Values{}
	form.Set("client_id", ClientID)
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", fmt.Sprintf("http://127.0.0.1:%d%s", port, CallbackPath))
	return requestToken(ctx, hc, stsBase, form, jwk, "authorization_code")
}

// PollTicket asks snap-manager whether the browser login finished. It returns
// (nil, nil) while the user has not completed the flow yet: only a response
// that carries a user name is treated as done.
func PollTicket(ctx context.Context, hc *http.Client, base, ticketID, secret string) (*TokenResponse, error) {
	target := strings.TrimRight(base, "/") + "/snap-manager" + LoginTicket +
		"?ticket_id=" + url.QueryEscape(ticketID) + "&secret=" + url.QueryEscape(secret)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("plugin-name", PluginName)
	req.Header.Set("plugin-version", PluginVersion)
	req.Header.Set("Content-Type", "application/json")
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("codearts ticket HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var out TokenResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("codearts ticket: parse response: %w", err)
	}
	// A business error must not look like "still waiting": the panel would poll
	// for the whole ten-minute window with no explanation.
	if out.ErrorCode != "" && out.ErrorCode != "0000" {
		return nil, fmt.Errorf("codearts ticket: %s (%s)", out.ErrorMessage, out.ErrorCode)
	}
	if out.Credentials.SecurityToken == "" && out.Credential.SecurityToken != "" {
		out.Credentials = Credential{
			AccessKeyID:     out.Credential.Access,
			SecretAccessKey: out.Credential.Secret,
			SecurityToken:   out.Credential.SecurityToken,
			Expiration:      out.Credential.ExpiresAt,
		}
	}
	if out.Credentials.SecurityToken == "" {
		return nil, nil // still waiting: the ticket has no credentials yet
	}
	// The identity may be absent here; the caller resolves it with the fresh
	// STS token (see CurrentUser).
	return &out, nil
}

// CurrentUser resolves the identity behind an STS token (account verification).
func CurrentUser(ctx context.Context, hc *http.Client, base string, cred Credential) (uid, name, domain string, err error) {
	raw, err := SignedGet(ctx, hc, strings.TrimRight(base, "/")+CurrentUserPath, cred, "")
	if err != nil {
		return "", "", "", err
	}
	var out struct {
		UserID   string `json:"user_id"`
		UserName string `json:"user_name"`
		DomainID string `json:"domain_id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", "", fmt.Errorf("parse current user: %w", err)
	}
	return out.UserID, out.UserName, out.DomainID, nil
}

// LoginBase is the snap-access root used by the login endpoints.
func LoginBase() string { return realm.CodeArtsChatBase }

// Session is one in-flight browser login: the values that must survive from the
// authorize URL to the ticket poll.
type Session struct {
	TicketID  string
	Secret    string
	Verifier  string
	Challenge string
	DPoP      DPoPKey
	Port      int
	Started   time.Time
}

// NewSession creates the state of one login attempt.
func NewSession(port int) (*Session, error) {
	ticketID, err := RandomHex(16)
	if err != nil {
		return nil, err
	}
	secret, err := RandomHex(16)
	if err != nil {
		return nil, err
	}
	verifier, challenge, err := PKCE()
	if err != nil {
		return nil, err
	}
	jwk, err := NewDPoPKey()
	if err != nil {
		return nil, err
	}
	return &Session{TicketID: ticketID, Secret: secret, Verifier: verifier, Challenge: challenge,
		DPoP: jwk, Port: port, Started: time.Now()}, nil
}

// URL is the portal link of this session.
func (s *Session) URL() string { return AuthorizeURL(s.TicketID, s.Challenge, s.Port) }

// IsPendingTicketError reports the "ticket not registered yet" answer from
// snap-manager. The portal only records a ticket once the browser has opened
// the authorize URL, so polling before that is a normal waiting state rather
// than a failure.
func IsPendingTicketError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "TM.00001001") ||
		strings.Contains(s, "无效 ticketId") ||
		strings.Contains(strings.ToLower(s), "invalid ticket")
}
