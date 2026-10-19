package ops

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"wb2api/internal/jsonx"
	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream/identity"
)

const (
	authStatePath    = "/v2/plugin/auth/state"
	authTokenPath    = "/v2/plugin/auth/token"
	loginAccountPath = "/v2/plugin/login/account"
	loginPending     = 11217
	loginTTL         = 10 * time.Minute
)

type loginInfo struct {
	created  time.Time
	realm    string
	platform string
}

// LoginStart begins the browser (OAuth device) login: the operator opens the
// returned URL, signs in, and the gateway polls for the token.
func (c *Client) LoginStart(ctx context.Context, r, platform string) (map[string]any, error) {
	if r != realm.Intl && r != realm.CN {
		r = realm.Intl
	}
	if platform == "" {
		platform = "CLI"
	}
	u := c.chatBase(r) + authStatePath + "?platform=" + url.QueryEscape(platform)
	o, err := c.loginCall(ctx, "POST", u, r, []byte("{}"))
	if err != nil {
		return nil, err
	}
	data, _ := o.Get("data").(*jsonx.Obj)
	if data == nil {
		return nil, fmt.Errorf("auth/state returned no data")
	}
	state, authURL := jsonx.PyStrIfTruthy(data.Get("state")), jsonx.PyStrIfTruthy(data.Get("authUrl"))
	if state == "" || authURL == "" {
		return nil, fmt.Errorf("auth/state returned no state/authUrl")
	}
	c.loginMu.Lock()
	if c.logins == nil {
		c.logins = map[string]loginInfo{}
	}
	for k, v := range c.logins { // forget stale attempts
		if c.now().Sub(v.created) > loginTTL {
			delete(c.logins, k)
		}
	}
	c.logins[state] = loginInfo{created: c.now(), realm: r, platform: platform}
	c.loginMu.Unlock()
	return map[string]any{"state": state, "authUrl": authURL, "realm": r, "platform": platform}, nil
}

func (c *Client) loginHeaders(r string) identity.Headers {
	cfg := realm.Get(r)
	return identity.Headers{
		identity.P("Content-Type", "application/json"),
		identity.P("Accept", "application/json, text/plain, */*"),
		identity.P("X-Requested-With", "XMLHttpRequest"),
		identity.P("User-Agent", billingUA(r)),
		identity.P("Origin", cfg.Origin),
		identity.P("Referer", cfg.Origin+"/"),
	}
}

// loginCall is an unauthenticated call (no account yet), sent direct.
func (c *Client) loginCall(ctx context.Context, method, u, r string, body []byte) (*jsonx.Obj, error) {
	ghost := pool.NewAccount(accounts.FromMap(map[string]any{"uid": "_login", "accessToken": "x", "realm": r}, "", c.now()))
	return c.doJSON(ctx, ghost, method, u, c.loginHeaders(r), body, 30*time.Second, 3)
}

// LoginPoll checks whether the browser login finished. On success the account
// is added to the pool (and, on the CN exit, checked in once).
func (c *Client) LoginPoll(ctx context.Context, state string) map[string]any {
	state = strings.TrimSpace(state)
	c.loginMu.Lock()
	info, ok := c.logins[state]
	c.loginMu.Unlock()
	if !ok {
		return map[string]any{"status": "unknown", "message": "state not recognised - start the login again"}
	}
	if c.now().Sub(info.created) > loginTTL {
		c.CancelLogin(state)
		return map[string]any{"status": "expired", "message": "login window expired - start again"}
	}
	r := info.realm
	o, err := c.loginCall(ctx, "GET", c.chatBase(r)+authTokenPath+"?state="+url.QueryEscape(state), r, nil)
	if err != nil {
		return map[string]any{"status": "pending", "message": "poll error: " + err.Error()}
	}
	if code := jsonx.PyStr(o.Get("code")); code == fmt.Sprint(loginPending) {
		return map[string]any{"status": "pending", "message": orStr(o.Get("msg"), "waiting for browser login")}
	} else if code != "0" {
		return map[string]any{"status": "error", "message": fmt.Sprintf("code=%s msg=%s", code, jsonx.PyStr(o.Get("msg")))}
	}
	data, _ := o.Get("data").(*jsonx.Obj)
	if data == nil {
		return map[string]any{"status": "pending", "message": "waiting for token"}
	}
	token := jsonx.PyStrIfTruthy(data.Get("accessToken"))
	if token == "" {
		return map[string]any{"status": "pending", "message": "waiting for token"}
	}
	uid := realm.JWTUID(token)
	nickname := ""
	headers := append(c.loginHeaders(r), identity.P("Authorization", "Bearer "+token))
	ghost := pool.NewAccount(accounts.FromMap(map[string]any{"uid": "_login", "accessToken": "x", "realm": r}, "", c.now()))
	if prof, perr := c.doJSON(ctx, ghost, "GET", c.chatBase(r)+loginAccountPath+"?state="+url.QueryEscape(state), headers, nil, 15*time.Second, 1); perr == nil {
		if pd, ok := prof.Get("data").(*jsonx.Obj); ok {
			nickname = jsonx.PyStrIfTruthy(pd.Get("nickname"))
		}
	}
	if nickname == "" {
		nickname = short(uid)
	}
	exp := realm.NormalizeEpoch(plainNum(data.Get("expiresAt")))
	if exp == 0 {
		exp = realm.JWTExp(token)
	}
	acc, err := c.AddAccount(map[string]any{
		"uid": uid, "nickname": nickname, "domain": orStr(data.Get("domain"), realm.Get(r).Domain), "realm": r,
		"platform": info.platform, "accessToken": token, "refreshToken": orStr(data.Get("refreshToken"), ""),
		"expiresAt": exp, "source": "oauth", "enabled": true,
	}, true)
	if err != nil {
		return map[string]any{"status": "error", "message": err.Error()}
	}
	if r == realm.CN {
		c.Checkin(ctx, acc, "account_add")
	}
	c.CancelLogin(state)
	return map[string]any{"status": "ok", "uid": acc.UID, "nickname": nickname}
}

func plainNum(v any) any { return plain(v) }

// CancelLogin forgets a pending login.
func (c *Client) CancelLogin(state string) bool {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	_, ok := c.logins[state]
	delete(c.logins, state)
	return ok
}

// AddAccount creates (or, with replace, overwrites) an account from a record
// map, persists it and puts it in the pool. A replaced account keeps its
// added-at time, balance, activity marks and proxy binding when the new
// document carries none of its own.
func (c *Client) AddAccount(m map[string]any, replace bool) (*pool.Account, error) {
	rec := accounts.FromMap(m, "", c.now())
	if rec.UID == "" {
		return nil, fmt.Errorf("cannot determine uid")
	}
	if rec.AccessToken == "" {
		return nil, fmt.Errorf("no accessToken")
	}
	if old := c.Pool.Get(rec.UID); old != nil {
		if !replace {
			return nil, fmt.Errorf("account %s already exists", rec.UID)
		}
		prev := old.Record()
		rec.AddedAt, rec.Path = prev.AddedAt, prev.Path
		if rec.Credits == nil {
			rec.Credits = prev.Credits
		}
		if rec.LastCheckin == nil {
			rec.LastCheckin = prev.LastCheckin
		}
		if rec.LastDailyChat == nil {
			rec.LastDailyChat = prev.LastDailyChat
		}
		if rec.ProxySlot == "" {
			rec.ProxySlot = prev.ProxySlot
		}
		if rec.ProxyLegacy == "" {
			rec.ProxyLegacy = prev.ProxyLegacy
		}
	}
	if c.Persist != nil {
		if err := c.Persist(rec); err != nil {
			return nil, err
		}
	}
	acc := c.Pool.Add(rec)
	return acc, nil
}
