package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"wb2api/internal/codeartsync"
	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream/codearts"
)

// codeartsLoginTTL bounds one browser login attempt. The portal session itself
// is short-lived; this only reaps abandoned entries.
const codeartsLoginTTL = 10 * time.Minute

// codeartsLogins holds the in-flight CodeArts login sessions. The zero value is
// usable so Operations needs no constructor.
type codeartsLogins struct {
	mu sync.Mutex
	m  map[string]*codearts.Session
}

func (c *codeartsLogins) start(sess *codearts.Session) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*codearts.Session{}
	}
	now := time.Now()
	for k, v := range c.m { // reap abandoned attempts
		if now.Sub(v.Started) > codeartsLoginTTL {
			delete(c.m, k)
		}
	}
	c.m[id] = sess
	return id
}

func (c *codeartsLogins) get(id string) (*codearts.Session, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.m[id]
	return s, ok
}

func (c *codeartsLogins) drop(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, id)
}

// startCodeArtsLogin answers the same shape as the WorkBuddy login: a state and
// an authorisation URL the panel opens in a browser.
func (o *Operations) startCodeArtsLogin() (map[string]any, error) {
	sess, err := codearts.NewSession(0)
	if err != nil {
		return nil, err
	}
	id := o.codeartsLogins.start(sess)
	return map[string]any{
		"state":   id,
		"authUrl": sess.URL(),
		"realm":   realm.CodeArts,
		"message": "用华为云账号打开链接完成登录，网关会自动领取凭据并落盘",
	}, nil
}

// pollCodeArtsLogin advances one login attempt: ticket polling needs no inbound
// callback, so a gateway behind NAT can still be authorised.
func (o *Operations) pollCodeArtsLogin(ctx context.Context, id string) map[string]any {
	sess, ok := o.codeartsLogins.get(id)
	if !ok {
		return map[string]any{"status": "unknown", "message": "登录会话不存在，请重新发起"}
	}
	if time.Since(sess.Started) > codeartsLoginTTL {
		o.codeartsLogins.drop(id)
		return map[string]any{"status": "expired", "message": "登录窗口已过期，请重新发起"}
	}
	base := codearts.LoginBase()
	if o.CodeArts != nil && o.CodeArts.Base != "" {
		base = o.CodeArts.Base
	}
	if o.CodeArtsLoginBase != "" {
		base = o.CodeArtsLoginBase
	}
	tok, err := codearts.PollTicket(ctx, nil, base, sess.TicketID, sess.Secret)
	if err != nil {
		return map[string]any{"status": "pending", "message": "轮询失败：" + err.Error()}
	}
	if tok == nil {
		return map[string]any{"status": "pending", "message": "等待浏览器完成登录"}
	}
	rec, err := o.addCodeArtsAccount(tok, sess)
	if err != nil {
		return map[string]any{"status": "error", "message": err.Error()}
	}
	o.codeartsLogins.drop(id)
	// Fill the catalogue right away; the background scan would otherwise take a
	// full tick before /v1/models shows anything.
	if o.CodeArts != nil {
		acc := o.Pool.Get(rec.UID)
		if acc != nil {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				if _, err := o.CodeArts.Discover(ctx, acc); err != nil {
					o.CodeArts.Log.Warn("codearts model discovery after login", "err", err)
				}
			}()
		}
	}
	return map[string]any{
		"status": "ok", "message": "登录成功", "uid": rec.UID, "nickname": rec.Nickname,
	}
}

// addCodeArtsAccount stores the credential of a finished login.
func (o *Operations) addCodeArtsAccount(tok *codearts.TokenResponse, sess *codearts.Session) (*accounts.Record, error) {
	uid := strings.TrimSpace(tok.UserID)
	if uid == "" {
		uid = strings.TrimSpace(tok.UserName)
	}
	if uid == "" {
		return nil, errors.New("登录返回的账号没有 user_id")
	}
	exp := tok.Expiry()
	if exp.IsZero() {
		exp = time.Now().Add(2 * time.Hour)
	}
	jwk := map[string]any{}
	for k, v := range sess.DPoP {
		jwk[k] = v
	}
	extra := map[string]any{
		"access_key_id":     tok.Credentials.AccessKeyID,
		"secret_access_key": tok.Credentials.SecretAccessKey,
		"security_token":    tok.Credentials.SecurityToken,
		"expiration":        tok.Credentials.Expiration,
		"client_id":         codearts.ClientID,
		"code_verifier":     sess.Verifier,
		"dpop_private_key":  jwk,
	}
	rec := &accounts.Record{
		UID: uid, Nickname: strings.TrimSpace(tok.UserName), Realm: realm.CodeArts,
		AccessToken: tok.Credentials.SecurityToken, RefreshToken: tok.RefreshToken,
		ExpiresAt: exp.Unix(), Source: "codearts-oauth", Platform: "codearts",
		Product: "workbuddy", // outbound body shaping is unused on this channel
		Extra:   map[string]any{codearts.ExtraKey: extra},
	}
	if existing := o.Pool.Get(uid); existing != nil {
		if next, ok := o.Pool.Update(uid, func(r *accounts.Record) {
			r.Nickname = rec.Nickname
			r.AccessToken = rec.AccessToken
			r.RefreshToken = rec.RefreshToken
			r.ExpiresAt = rec.ExpiresAt
			r.Source = rec.Source
			r.Realm = rec.Realm
			r.Extra = rec.Extra
		}); ok {
			rec = next
		}
	} else {
		o.Pool.Add(rec)
	}
	if o.Persist != nil {
		if err := o.Persist(rec); err != nil {
			return rec, err
		}
	}
	return rec, nil
}

// codeartsModelsRefresh re-runs discovery for one account or the whole channel.
func (o *Operations) codeartsModelsRefresh(w http.ResponseWriter, r *http.Request) {
	if o.CodeArts == nil {
		writeErr(w, http.StatusNotImplemented, "CodeArts 渠道未启用")
		return
	}
	var in struct {
		UID string `json:"uid"`
	}
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	targets := []*pool.Account{}
	if uid := strings.TrimSpace(in.UID); uid != "" {
		acc := o.Pool.Get(uid)
		if acc == nil {
			writeErr(w, http.StatusNotFound, "no such account")
			return
		}
		targets = append(targets, acc)
	} else {
		for _, acc := range o.Pool.All() {
			if acc != nil && realm.IsCodeArts(acc.Record().Realm) {
				targets = append(targets, acc)
			}
		}
	}
	type result struct {
		UID    string            `json:"uid"`
		OK     bool              `json:"ok"`
		Models []string          `json:"models"`
		Errors map[string]string `json:"errors,omitempty"`
		Error  string            `json:"error,omitempty"`
	}
	out := make([]result, 0, len(targets))
	failed := 0
	for _, acc := range targets {
		dis, err := o.CodeArts.Discover(ctx, acc)
		row := result{UID: acc.UID, Models: []string{}, Errors: dis.Errors}
		for _, mi := range dis.Models {
			row.Models = append(row.Models, mi.ID)
		}
		row.OK = err == nil
		if err != nil {
			row.Error = err.Error()
			failed++
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": failed == 0, "count": len(out), "accounts": out})
}

// codeartsChannel returns the shared service (nil-safe).
func (o *Operations) codeartsChannel() *codeartsync.Service { return o.CodeArts }
