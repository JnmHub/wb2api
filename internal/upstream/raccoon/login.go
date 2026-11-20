package raccoon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"wb2api/internal/upstream/identity"
)

// Login modes. "desktop" makes the official page answer with a
// office-raccoon:// redirect (the browser address bar never shows the code);
// "web" makes it append the code to the redirect URL instead, which is the only
// mode usable from a server deployment.
//
// The authorize page only accepts an http redirect to a loopback host with an
// explicit port (measured 2026-10), so the web mode is a localhost story.
const (
	ModeDesktop = LoginSourceDesktop
	ModeWeb     = LoginSourceWeb
)

// LoginState returns an unpredictable state value (32 hex characters). The
// callback is a custom protocol any local program can forge, so the state is
// what proves "this code came back from the attempt this process started".
func LoginState() string {
	var buf [16]byte
	rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

// AuthorizeURL builds the sign-in page URL.
//
//	mode = desktop -> {origin}/authorize?login_source=desktop&appname=…&state=…
//	mode = web     -> same with login_source=web and &redirect=<callback>
func AuthorizeURL(state, mode, redirect string) string {
	q := url.Values{}
	q.Set("login_source", LoginSourceDesktop)
	if mode == ModeWeb {
		q.Set("login_source", LoginSourceWeb)
	}
	// appname is a Chinese literal: append_pair/Values encode it correctly.
	q.Set("appname", AppName)
	q.Set("state", state)
	if mode == ModeWeb && strings.TrimSpace(redirect) != "" {
		q.Set("redirect", strings.TrimSpace(redirect))
	}
	base := strings.TrimRight(Origin(), "/")
	return base + AuthorizePath + "?" + q.Encode()
}

// IsLoopbackHTTP reports whether a panel URL may be used as the web-mode
// redirect target: http scheme, loopback host, explicit port. The upstream
// rejects everything else with "授权链接无效", which reads to the user as a
// broken panel link.
func IsLoopbackHTTP(scheme, host, port string) bool {
	if !strings.EqualFold(scheme, "http") || strings.TrimSpace(port) == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "127.0.0.1", "localhost", "[::1]", "::1":
		return true
	}
	return false
}

// ErrCallbackState marks a callback whose state does not match the attempt.
var ErrCallbackState = fmt.Errorf("登录回调 state 校验失败，请重新发起登录")

// CodeFromInput extracts an authorization code from whatever the operator could
// copy: the whole office-raccoon:// callback URL, an http callback that failed
// to load (the code is still in the address bar), or the bare code.
//
// A bare code cannot be checked against the attempt state (it carries none), so
// callers must still require a known state before exchanging it.
func CodeFromInput(raw, expectedState string) (string, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return "", fmt.Errorf("缺少授权码")
	}
	if strings.Contains(text, CallbackScheme+"://") {
		return ParseCallback(text, expectedState)
	}
	if strings.Contains(text, "://") {
		// The web-mode callback is an http URL of this gateway. It is not the
		// official scheme, so only the query is authoritative: require a code
		// and the state this attempt started with.
		u, err := url.Parse(text)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return "", fmt.Errorf("登录回调地址无效")
		}
		q := u.Query()
		code := firstNonEmpty(q.Get("code"), q.Get("authorization_code"))
		if code == "" {
			return "", fmt.Errorf("登录回调里没有授权码")
		}
		if state := q.Get("state"); state == "" || state != expectedState {
			return "", ErrCallbackState
		}
		return code, nil
	}
	// Not a URL: treat it as the code itself. Reject whitespace so a mangled
	// callback is not silently sent upstream.
	if strings.ContainsAny(text, " \t\r\n") {
		return "", fmt.Errorf("登录回调地址无效")
	}
	if len(text) > 8192 {
		return "", fmt.Errorf("授权码过长")
	}
	return text, nil
}

// ParseCallback extracts the authorization code from a desktop callback URL.
//
// Every component is compared as a whole (scheme, host, path): a prefix check
// would accept office-raccoon://auth/callback@evil.com.
func ParseCallback(rawURL, expectedState string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", fmt.Errorf("登录回调地址无效")
	}
	if !strings.EqualFold(u.Scheme, CallbackScheme) ||
		!strings.EqualFold(u.Host, CallbackHost) ||
		u.Path != CallbackPath {
		return "", fmt.Errorf("登录回调地址无效")
	}
	q := u.Query()
	code := strings.TrimSpace(q.Get("code"))
	if code == "" {
		// The web mode names the parameter authorization_code.
		code = strings.TrimSpace(q.Get("authorization_code"))
	}
	if code == "" {
		return "", fmt.Errorf("登录回调里没有授权码")
	}
	if len(code) > 8192 {
		return "", fmt.Errorf("授权码过长")
	}
	state := strings.TrimSpace(q.Get("state"))
	if state == "" {
		return "", fmt.Errorf("登录回调里没有 state，无法确认这次登录由本机发起")
	}
	if len(state) > 512 {
		return "", fmt.Errorf("登录回调 state 过长")
	}
	if state != expectedState {
		return "", ErrCallbackState
	}
	return code, nil
}

// ExchangeCode trades a one-off authorization code for credentials.
//
// Codes expire quickly (business code 200035): the error text must tell the
// operator to start over, not to wait for a retry.
func (c *Client) ExchangeCode(ctx context.Context, code string) (Credential, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return Credential{}, fmt.Errorf("缺少授权码")
	}
	if len(code) > 8192 {
		return Credential{}, fmt.Errorf("授权码过长")
	}
	headers := identityJSONHeaders()
	body := map[string]any{"authorization_code": code}
	resp, err := c.Do(ctx, http.MethodPost, c.authBase()+ExchangeCodePath, headers, body)
	if err != nil {
		return Credential{}, fmt.Errorf("网页登录换取凭证失败: %w", err)
	}
	if resp.Code != nil && *resp.Code == CodeAuthCodeExpired {
		return Credential{}, fmt.Errorf("授权码已失效，请重新发起网页登录")
	}
	token := StripBearer(stringOf(resp.Data, "access_token", "accessToken"))
	if !resp.OK() || token == "" {
		msg := resp.Message
		if resp.Status >= 400 && resp.Status < 500 {
			return Credential{}, fmt.Errorf("%s", msg)
		}
		return Credential{}, fmt.Errorf("网页登录换取凭证失败（HTTP %d）: %s", resp.Status, msg)
	}
	cred := Credential{
		Token:        token,
		RefreshToken: StripBearer(stringOf(resp.Data, "refresh_token", "refreshToken")),
		IAM:          IsIAM(token),
		UserID:       UserID(token),
		ExpiresAt:    Expiry(token),
		OrgCode:      stringOf(resp.Data, "office_identity", "officeIdentity"),
		OrgName:      stringOf(resp.Data, "office_org_name", "officeOrgName"),
		OrgRole:      stringOf(resp.Data, "office_org_role", "officeOrgRole"),
	}
	if cred.IAM {
		if cred.ExpiresAt.IsZero() || ValidateIAM(token) != nil {
			return Credential{}, ErrNeedsLogin
		}
		cred.RefreshToken = ""
		// The profile is authoritative for the id and the office identity: an
		// IAM token's sub is the platform id, but the organisation may differ.
		if profile, perr := c.UserInfo(ctx, token); perr == nil {
			if id := ProfileUserID(profile); id != "" {
				cred.UserID = id
			}
			if cred.OrgCode == "" {
				cred.OrgCode = firstNonEmpty(stringOf(profile, "officeIdentity", "office_identity"), "personal")
			}
			if cred.OrgName == "" {
				cred.OrgName = stringOf(profile, "officeOrgName", "office_org_name")
			}
			if cred.OrgRole == "" {
				cred.OrgRole = stringOf(profile, "officeOrgRole", "office_org_role")
			}
		}
	}
	if cred.UserID == "" {
		return Credential{}, fmt.Errorf("小浣熊凭据缺少有效的账号 ID")
	}
	return cred, nil
}

// ProfileUserID accepts both the string and integer forms of the profile ID.
func ProfileUserID(profile map[string]any) string {
	for _, field := range []string{"id", "userId", "user_id"} {
		switch id := profile[field].(type) {
		case string:
			if s := strings.TrimSpace(id); s != "" {
				return s
			}
		case json.Number:
			if n, err := id.Int64(); err == nil && n > 0 {
				return strconv.FormatInt(n, 10)
			}
		case float64:
			if id > 0 && id <= 1<<53-1 && float64(int64(id)) == id {
				return strconv.FormatInt(int64(id), 10)
			}
		}
	}
	return ""
}

// identityJSONHeaders is the minimal header set of the code-exchange call: the
// upstream takes no bearer and no client identity there.
func identityJSONHeaders() identity.Headers {
	return identity.Headers{{Name: "Content-Type", Value: "application/json"}}
}

// CallbackURL is the desktop callback the official page uses in desktop mode.
// It is returned to the panel purely as a hint ("this is where the browser will
// try to go").
func CallbackURL() string {
	return CallbackScheme + "://" + CallbackHost + CallbackPath
}
