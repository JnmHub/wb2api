package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
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

// codeartsLogin is one in-flight browser login.
//
// Each attempt gets its own loopback listener, so the portal's callback lands on
// exactly the session that started it: the authorize URL carries that port and
// the code/verifier pair can never be mixed up with another tab's attempt.
type codeartsLogin struct {
	mu      sync.Mutex
	session *codearts.Session
	secret  string
	record  *accounts.Record
	failure string
	code    string
	lastErr string
	done    bool
	started time.Time
	srv     *http.Server
}

func (l *codeartsLogin) setSecret(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if strings.TrimSpace(s) != "" {
		l.secret = strings.TrimSpace(s)
	}
}

func (l *codeartsLogin) pollSecret() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.secret != "" {
		return l.secret
	}
	return l.session.Secret
}

// claimCode records the authorization code and reports whether this exact code
// was already redeemed (a browser reload).
func (l *codeartsLogin) claimCode(code string) (repeat bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.code == code && l.done {
		return true
	}
	l.code = code
	return false
}

func (l *codeartsLogin) noteFailure(msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastErr = msg
}

func (l *codeartsLogin) lastError() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastErr
}

func (l *codeartsLogin) finish(rec *accounts.Record, failure string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.record, l.failure, l.done = rec, failure, true
}

func (l *codeartsLogin) state() (rec *accounts.Record, failure string, done bool, started time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.record, l.failure, l.done, l.started
}

// attach keeps the per-attempt callback server so it can be shut down.
func (l *codeartsLogin) attach(srv *http.Server) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.srv = srv
}

func (l *codeartsLogin) shutdown() {
	l.mu.Lock()
	srv := l.srv
	l.srv = nil
	l.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}

// codeartsLogins holds the in-flight CodeArts login sessions. The zero value is
// usable so Operations needs no constructor.
type codeartsLogins struct {
	mu   sync.Mutex
	m    map[string]*codeartsLogin
	last string
}

func (c *codeartsLogins) start(sess *codearts.Session) (string, *codeartsLogin) {
	var b [8]byte
	_, _ = rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	l := &codeartsLogin{session: sess, started: time.Now()}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*codeartsLogin{}
	}
	now := time.Now()
	for k, v := range c.m { // reap abandoned attempts
		if _, _, done, started := v.state(); !done && now.Sub(started) > codeartsLoginTTL {
			delete(c.m, k)
			go v.shutdown()
		}
	}
	c.m[id] = l
	c.last = id
	return id, l
}

func (c *codeartsLogins) get(id string) (*codeartsLogin, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.m[id]
	return s, ok
}

func (c *codeartsLogins) newest() *codeartsLogin {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == "" {
		return nil
	}
	return c.m[c.last]
}

func (c *codeartsLogins) drop(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, id)
	if c.last == id {
		c.last = ""
	}
}

// panelPort is the port the browser reaches this gateway on.
func panelPort(r *http.Request) int {
	host := r.Host
	if i := strings.LastIndex(host, ":"); i >= 0 {
		if n, err := strconv.Atoi(host[i+1:]); err == nil && n > 0 {
			return n
		}
	}
	if r.TLS != nil {
		return 443
	}
	return 80
}

// listenLoopback opens the callback listener of one login attempt. When it
// cannot bind (restricted environment), the panel port is used instead and the
// shared /oauth/callback route takes over.
func listenLoopback() (net.Listener, int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, err
	}
	return ln, ln.Addr().(*net.TCPAddr).Port, nil
}

// startCodeArtsLogin answers the same shape as the WorkBuddy login: a state and
// an authorisation URL the panel opens in a browser.
func (o *Operations) startCodeArtsLogin(r *http.Request) (map[string]any, error) {
	port := panelPort(r)
	ln, cbPort, err := listenLoopback()
	if err == nil {
		port = cbPort
	}
	sess, err := codearts.NewSession(port)
	if err != nil {
		if ln != nil {
			_ = ln.Close()
		}
		return nil, err
	}
	id, login := o.codeartsLogins.start(sess)
	if ln != nil {
		mux := http.NewServeMux()
		mux.HandleFunc(codearts.CallbackPath, func(w http.ResponseWriter, req *http.Request) {
			o.codeartsCallbackFor(login, w, req)
		})
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		login.attach(srv)
		go func() { _ = srv.Serve(ln) }()
		go func() {
			time.Sleep(codeartsLoginTTL)
			login.shutdown()
		}()
	}
	return map[string]any{
		"state":    id,
		"authUrl":  sess.URL(),
		"realm":    realm.CodeArts,
		"callback": fmt.Sprintf("http://127.0.0.1:%d%s", sess.Port, codearts.CallbackPath),
		"message":  "用华为云账号打开链接完成登录；浏览器会回调本机网关，网关自动领取凭据并落盘",
	}, nil
}

// codeartsSTSBase is the STS root the code exchange and the refresh chain use.
func (o *Operations) codeartsSTSBase() string {
	if o.CodeArtsSTSBase != "" {
		return o.CodeArtsSTSBase
	}
	return codearts.STSHost
}

// codeartsBase is the snap-access root the login flow talks to (ticket poll,
// catalogue, check-in).
func (o *Operations) codeartsBase() string {
	if o.CodeArts != nil && o.CodeArts.Base != "" {
		return o.CodeArts.Base
	}
	if o.CodeArtsLoginBase != "" {
		return o.CodeArtsLoginBase
	}
	return codearts.LoginBase()
}

// pollCodeArtsLogin advances one login attempt.
func (o *Operations) pollCodeArtsLogin(ctx context.Context, id string) map[string]any {
	sess, ok := o.codeartsLogins.get(id)
	if !ok {
		return map[string]any{"status": "unknown", "message": "登录会话不存在，请重新发起"}
	}
	rec, failure, done, started := sess.state()
	if done {
		sess.shutdown()
		if rec == nil {
			return map[string]any{"status": "error", "message": orDefault(failure, "登录失败")}
		}
		o.finishCodeArtsLogin(rec)
		o.codeartsLogins.drop(id)
		return map[string]any{"status": "ok", "message": "登录成功", "uid": rec.UID, "nickname": rec.Nickname}
	}
	if time.Since(started) > codeartsLoginTTL {
		sess.shutdown()
		o.codeartsLogins.drop(id)
		return map[string]any{"status": "expired", "message": "登录窗口已过期，请重新发起"}
	}
	tok, err := codearts.PollTicket(ctx, nil, o.codeartsBase(), sess.session.TicketID, sess.pollSecret())
	if err != nil {
		if codearts.IsPendingTicketError(err) {
			// The portal has not registered the ticket yet: that is the normal
			// state until the browser opens the authorize URL.
			return map[string]any{"status": "pending", "message": "等待浏览器打开登录链接并完成登录"}
		}
		return map[string]any{"status": "pending", "message": "轮询出错，继续重试：" + err.Error()}
	}
	if tok == nil {
		msg := "等待浏览器完成登录"
		if last := sess.lastError(); last != "" {
			msg = "授权码交换失败（" + last + "），仍在尝试票据轮询"
		}
		return map[string]any{"status": "pending", "message": msg}
	}
	rec, aerr := o.addCodeArtsAccount(ctx, tok, sess.session)
	if aerr != nil {
		return map[string]any{"status": "error", "message": aerr.Error()}
	}
	sess.finish(rec, "")
	sess.shutdown()
	o.finishCodeArtsLogin(rec)
	o.codeartsLogins.drop(id)
	return map[string]any{"status": "ok", "message": "登录成功", "uid": rec.UID, "nickname": rec.Nickname}
}

// codeartsCallback is the shared fallback route (used when the per-attempt
// listener could not be opened). It is deliberately unauthenticated.
func (o *Operations) codeartsCallback(w http.ResponseWriter, r *http.Request) {
	sess := o.codeartsLogins.newest()
	if sess == nil {
		writeErr(w, http.StatusBadRequest, "没有进行中的登录，请在面板重新发起")
		return
	}
	o.codeartsCallbackFor(sess, w, r)
}

// codeartsCallbackFor handles the portal's two callbacks for one session: first
// the secret (+redirect), then the authorization code.
func (o *Operations) codeartsCallbackFor(l *codeartsLogin, w http.ResponseWriter, r *http.Request) {
	code, secret, redirect := r.URL.Query().Get("code"), r.URL.Query().Get("secret"), r.URL.Query().Get("redirect")
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		code = firstNonEmpty(code, r.PostForm.Get("code"))
		secret = firstNonEmpty(secret, r.PostForm.Get("secret"))
		redirect = firstNonEmpty(redirect, r.PostForm.Get("redirect"))
	}
	if secret != "" {
		l.setSecret(secret)
	}
	if code == "" {
		if redirect != "" {
			// The first callback only carries the secret and a redirect; the
			// portal's login chain breaks if this hop is not followed.
			http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)
			return
		}
		writeCodeArtsPage(w, http.StatusOK, "登录进行中", "可以关闭本页，回到面板继续等待。")
		return
	}
	if l.claimCode(code) {
		// A reload of the same callback: the code is single-use, so report the
		// remembered outcome instead of asking STS again.
		if rec, failure, done, _ := l.state(); done {
			if rec != nil {
				writeCodeArtsPage(w, http.StatusOK, "登录成功", "账号 "+rec.UID+" 已在网关中，可以关闭本页。")
			} else {
				writeCodeArtsPage(w, http.StatusBadGateway, "登录失败", failure)
			}
			return
		}
	}
	rec, err := o.completeCodeArtsLogin(context.WithoutCancel(r.Context()), l, code)
	if err != nil {
		detail := fmt.Sprintf("%s<br><br>redirect_uri=http://127.0.0.1:%d%s，授权码 %d 字符。后台仍在尝试票据轮询，可回到面板观察结果。",
			err.Error(), l.session.Port, codearts.CallbackPath, len(code))
		writeCodeArtsPage(w, http.StatusBadGateway, "登录失败", detail)
		return
	}
	writeCodeArtsPage(w, http.StatusOK, "登录成功", "账号 "+rec.UID+" 已写入网关，可以关闭本页。")
}

// completeCodeArtsLogin exchanges one authorization code and stores the account.
// Both the loopback callback and the paste endpoint end up here.
func (o *Operations) completeCodeArtsLogin(ctx context.Context, l *codeartsLogin, code string) (*accounts.Record, error) {
	tok, err := codearts.ExchangeCode(ctx, nil, o.codeartsSTSBase(), code, l.session.Verifier, l.session.Port, l.session.DPoP)
	if err != nil {
		// A failed exchange does not end the attempt: the portal may still have
		// registered a ticket, so the panel's poll keeps running.
		l.noteFailure(err.Error())
		if o.CodeArts != nil {
			o.CodeArts.Log.Warn("codearts code exchange failed", "err", err)
		}
		return nil, err
	}
	rec, err := o.addCodeArtsAccount(ctx, tok, l.session)
	if err != nil {
		l.noteFailure(err.Error())
		return nil, err
	}
	l.finish(rec, "")
	o.finishCodeArtsLogin(rec)
	return rec, nil
}

// codeartsLoginComplete finishes a login from the callback URL the operator
// pasted into the panel.
//
// The portal always redirects the browser to http://127.0.0.1:<loopback port>,
// which is the gateway's own machine. On a server deployment the address bar of
// the operator's laptop cannot open it, so the code is carried back by hand.
func (o *Operations) codeartsLoginComplete(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Attempt string `json:"attempt"`
		URL     string `json:"url"`
	}
	if !readBody(w, r, &b) {
		return
	}
	code, secret := parseCodeArtsCallback(b.URL)
	if secret != "" {
		if l, ok := o.codeartsLogins.get(strings.TrimSpace(b.Attempt)); ok {
			l.setSecret(secret)
		}
	}
	if code == "" {
		msg := "地址里没有 code 参数；请把浏览器地址栏里 oauth/callback?code=… 这一整行粘回来"
		if secret != "" {
			msg = "已收到 secret，请继续把带 code= 的那一次回调地址粘进来"
		}
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	id := strings.TrimSpace(b.Attempt)
	l, ok := o.codeartsLogins.get(id)
	if !ok {
		writeErr(w, http.StatusBadRequest, "登录会话已失效，请重新发起登录")
		return
	}
	if rec, failure, done, _ := l.state(); done {
		if rec != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": rec.UID, "nickname": rec.Nickname, "replayed": true})
			return
		}
		writeErr(w, http.StatusBadGateway, orDefault(failure, "登录失败"))
		return
	}
	l.claimCode(code)
	rec, err := o.completeCodeArtsLogin(context.WithoutCancel(r.Context()), l, code)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "STS5.1805") || strings.Contains(msg, "invalid authorization code") {
			msg = "授权码已失效，或与本次登录会话不匹配（授权码几分钟就过期，且只能用在发起它的那次登录上）。请重新点「开始登录」，登录完成后立刻把地址栏那一行粘回来。"
		}
		writeErr(w, http.StatusBadGateway, msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": rec.UID, "nickname": rec.Nickname})
}

// parseCodeArtsCallback accepts a full URL, a bare query string or just the code.
func parseCodeArtsCallback(raw string) (code, secret string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	if u, err := url.Parse(raw); err == nil && (u.Scheme != "" || u.Host != "") {
		// A URL without a query is not a callback: keep it out of the exchange.
		if u.RawQuery != "" {
			q := u.Query()
			return strings.TrimSpace(q.Get("code")), strings.TrimSpace(q.Get("secret"))
		}
		return "", ""
	}
	if strings.Contains(raw, "=") {
		if q, err := url.ParseQuery(strings.TrimPrefix(raw, "?")); err == nil {
			return strings.TrimSpace(q.Get("code")), strings.TrimSpace(q.Get("secret"))
		}
	}
	return raw, ""
}

// finishCodeArtsLogin fills the catalogue after a successful login.
func (o *Operations) finishCodeArtsLogin(rec *accounts.Record) {
	if rec == nil || o.CodeArts == nil {
		return
	}
	acc := o.Pool.Get(rec.UID)
	if acc == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := o.CodeArts.Discover(ctx, acc); err != nil {
			o.CodeArts.Log.Warn("codearts model discovery after login", "err", err)
		}
	}()
}

func writeCodeArtsPage(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, "<!doctype html><meta charset=utf-8><title>"+title+"</title>"+
		"<body style=\"font-family:system-ui;padding:48px;text-align:center\"><h2>"+title+"</h2><p>"+detail+"</p></body>")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// addCodeArtsAccount stores the credential of a finished login.
//
// The token endpoint sometimes answers with credentials only, no identity: then
// the account id is resolved with the fresh STS token through snap-manager's
// current/user, which is exactly what that endpoint is for.
func (o *Operations) addCodeArtsAccount(ctx context.Context, tok *codearts.TokenResponse, sess *codearts.Session) (*accounts.Record, error) {
	uid := strings.TrimSpace(tok.UserID)
	if uid == "" {
		uid = strings.TrimSpace(tok.UserName)
	}
	if uid == "" {
		cred := codearts.Credential{
			AccessKeyID:     tok.Credentials.AccessKeyID,
			SecretAccessKey: tok.Credentials.SecretAccessKey,
			SecurityToken:   tok.Credentials.SecurityToken,
		}
		id, name, _, err := codearts.CurrentUser(ctx, nil, o.codeartsBase(), cred)
		if err != nil {
			return nil, fmt.Errorf("登录返回没有 user_id，且无法用 STS 令牌查询账号身份: %w", err)
		}
		uid, tok.UserID, tok.UserName = strings.TrimSpace(id), id, name
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
