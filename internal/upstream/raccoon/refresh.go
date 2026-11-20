package raccoon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"wb2api/internal/upstream/identity"
)

// RefreshRateLimit preserves the server's Retry-After without including
// upstream error text that might echo credentials.
type RefreshRateLimit struct {
	RetryAfter string
}

func (e *RefreshRateLimit) Error() string { return "小浣熊刷新接口限流（HTTP 429）" }

// Refresh exchanges the credential's refresh token for a new access token.
//
// It is deliberately narrow: the upstream takes a single body field and the old
// token as the bearer, with no grant_type, client id, PKCE or DPoP. That is
// also why an IAM credential cannot be refreshed at all.
func (c *Client) Refresh(ctx context.Context, cred Credential) (Credential, error) {
	if cred.IAM {
		return Credential{}, ErrNeedsLogin
	}
	if cred.Token == "" {
		return Credential{}, ErrNoCredential
	}
	if cred.RefreshToken == "" {
		return Credential{}, ErrNoRefreshToken
	}
	headers := identity.Headers{{Name: "Authorization", Value: "Bearer " + cred.Token}}
	body := map[string]any{"refresh_token": cred.RefreshToken}
	resp, err := c.Do(ctx, http.MethodPost, c.authBase()+RefreshPath, headers, body)
	if err != nil {
		return Credential{}, fmt.Errorf("小浣熊刷新 token 请求失败: %w", err)
	}
	if resp.Status == http.StatusTooManyRequests {
		return Credential{}, &RefreshRateLimit{RetryAfter: resp.RetryAfter}
	}
	if resp.TokenExpired() {
		return Credential{}, ErrNeedsLogin
	}
	if !resp.OK() {
		return Credential{}, fmt.Errorf("小浣熊刷新接口失败（HTTP %d）", resp.Status)
	}
	token := StripBearer(stringOf(resp.Data, "access_token", "accessToken"))
	if token == "" {
		return Credential{}, errors.New("小浣熊刷新响应缺少 access token")
	}
	next := cred
	next.Token = token
	next.IAM = IsIAM(token)
	next.ExpiresAt = Expiry(token)
	if next.IAM {
		// The upstream migrated this account to IAM; the legacy refresh token
		// is dead and must not be sent again.
		next.RefreshToken = ""
		if profile, perr := c.UserInfo(ctx, token); perr == nil {
			if id := stringOf(profile, "id", "userId"); id != "" {
				next.UserID = id
			}
			if org := firstNonEmpty(stringOf(profile, "officeIdentity", "office_identity"), next.OrgCode); org != "" {
				next.OrgCode = org
			}
		}
	} else if rt := StripBearer(stringOf(resp.Data, "refresh_token", "refreshToken")); rt != "" {
		next.RefreshToken = rt
	}
	if next.UserID == "" {
		next.UserID = UserID(token)
	}
	return next, nil
}

// UserInfo returns the platform profile of a token. Business code 200901 means
// the account has to confirm its first IAM migration once, so the call is
// retried after POST /iam/confirm.
func (c *Client) UserInfo(ctx context.Context, token string) (map[string]any, error) {
	profile, code, err := c.userInfoOnce(ctx, token)
	if err != nil {
		return nil, err
	}
	if code == CodeIAMConfirmRequired {
		if err := c.confirmIAM(ctx, token); err != nil {
			return nil, err
		}
		profile, code, err = c.userInfoOnce(ctx, token)
		if err != nil {
			return nil, err
		}
		if code != 0 {
			return nil, fmt.Errorf("小浣熊账号资料查询失败: 业务码 %d", code)
		}
	}
	if profile == nil {
		return nil, errors.New("小浣熊账号资料响应结构异常")
	}
	return profile, nil
}

// userInfoOnce performs one profile read and returns the business code so the
// caller can tell "needs IAM confirmation" (200901) from a real failure.
func (c *Client) userInfoOnce(ctx context.Context, token string) (map[string]any, int, error) {
	headers := authHeaders(token, "")
	resp, err := c.Do(ctx, http.MethodGet, c.authBase()+UserInfoPath, headers, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("小浣熊账号资料查询失败: %w", err)
	}
	if resp.TokenExpired() {
		return nil, 0, ErrNeedsLogin
	}
	code := 0
	if resp.Code != nil {
		code = *resp.Code
	}
	if resp.Status >= http.StatusBadRequest {
		return nil, code, fmt.Errorf("小浣熊账号资料查询失败（HTTP %d）: %s", resp.Status, resp.Message)
	}
	if code != CodeOK && code != CodeIAMConfirmRequired {
		return nil, code, fmt.Errorf("小浣熊账号资料查询失败: %s", resp.Message)
	}
	if resp.Data == nil {
		return nil, code, nil
	}
	return resp.Data, code, nil
}

func (c *Client) confirmIAM(ctx context.Context, token string) error {
	headers := authHeaders(token, "")
	resp, err := c.Do(ctx, http.MethodPost, c.authBase()+IAMConfirmPath, headers, map[string]any{})
	if err != nil {
		return fmt.Errorf("小浣熊 IAM 确认失败: %w", err)
	}
	if resp.TokenExpired() {
		return ErrNeedsLogin
	}
	if !resp.OK() {
		return fmt.Errorf("小浣熊 IAM 确认失败: %s", resp.Message)
	}
	return nil
}

// stringOf reads the first non-empty string among keys.
func stringOf(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
