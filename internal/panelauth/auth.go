package panelauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// CookieName is the session cookie.
	CookieName = "wb_session"
	// CSRFHeader must carry the session's CSRF token on state-changing requests.
	CSRFHeader = "X-WB-CSRF"
	// LegacyTokenHeader is the old bearer header; accepted for scripts. It is
	// not ambient (browsers never add it), so it needs no CSRF token.
	LegacyTokenHeader = "X-Panel-Token"

	loginWindow    = time.Minute
	loginMaxFails  = 5
	minPasswordLen = 4
)

// Passwords is the panel password backend (settings.Store implements it).
type Passwords interface {
	VerifyPanelPassword(password string) bool
	SetPanelPassword(password string) error
	PanelPasswordIsDefault() bool
}

// Auth wires login endpoints and the Require middleware.
type Auth struct {
	Store   *Store
	Pass    Passwords
	Proxies TrustedProxies
	// RequireChange refuses the panel while the default password is in use.
	RequireChange bool
	// FailDelay slows down each failed login (default 500 ms).
	FailDelay time.Duration
	Now       func() time.Time

	mu       sync.Mutex
	attempts map[string][]time.Time
}

type ctxKey struct{}

// SessionFrom returns the session attached by Require.
func SessionFrom(ctx context.Context) (Session, bool) {
	s, ok := ctx.Value(ctxKey{}).(Session)
	return s, ok
}

func (a *Auth) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg, "type": "invalid_request_error"}})
}

func isJSON(r *http.Request) bool {
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]))
	return ct == "application/json"
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if !isJSON(r) {
		writeErr(w, http.StatusUnsupportedMediaType, "json_required", "Content-Type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_body", "cannot read request body")
		return false
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return true
	}
	if err := json.Unmarshal(b, v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", "request body must be valid JSON")
		return false
	}
	return true
}

func (a *Auth) setCookie(w http.ResponseWriter, r *http.Request, token string, s Session) {
	maxAge := int(s.Expires.Sub(a.now()).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: token, Path: "/", MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.Proxies.IsHTTPS(r),
	})
}

func (a *Auth) clearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.Proxies.IsHTTPS(r),
	})
}

// tokenOf extracts the credential and whether it came from the cookie.
func tokenOf(r *http.Request) (token string, viaCookie bool) {
	if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
		return c.Value, true
	}
	return strings.TrimSpace(r.Header.Get(LegacyTokenHeader)), false
}

// ----------------------------------------------------------------- rate limit

// checkRate returns how long the caller must wait (0 = allowed).
func (a *Auth) checkRate(key string) time.Duration {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.attempts == nil {
		a.attempts = map[string][]time.Time{}
	}
	for k, list := range a.attempts { // prune
		keep := list[:0]
		for _, t := range list {
			if now.Sub(t) < loginWindow {
				keep = append(keep, t)
			}
		}
		if len(keep) == 0 {
			delete(a.attempts, k)
		} else {
			a.attempts[k] = keep
		}
	}
	list := a.attempts[key]
	if len(list) >= loginMaxFails {
		return loginWindow - now.Sub(list[0])
	}
	return 0
}

func (a *Auth) noteFail(key string) {
	a.mu.Lock()
	a.attempts[key] = append(a.attempts[key], a.now())
	a.mu.Unlock()
}

func (a *Auth) clearFails(key string) {
	a.mu.Lock()
	delete(a.attempts, key)
	a.mu.Unlock()
}

// ------------------------------------------------------------------ endpoints

// Register mounts the session endpoints under prefix (e.g. "/api/v2/session").
func (a *Auth) Register(mux *http.ServeMux, prefix string) {
	mux.HandleFunc("POST "+prefix+"/login", a.login)
	mux.HandleFunc("POST "+prefix+"/logout", a.logout)
	mux.HandleFunc("GET "+prefix, a.status)
	mux.Handle("POST "+prefix+"/password", a.Require(http.HandlerFunc(a.changePassword)))
	mux.Handle("GET "+prefix+"/devices", a.Require(http.HandlerFunc(a.devices)))
	mux.Handle("DELETE "+prefix+"/devices/{id}", a.Require(http.HandlerFunc(a.revokeDevice)))
	mux.Handle("POST "+prefix+"/devices/revoke-others", a.Require(http.HandlerFunc(a.revokeOthers)))
}

func (a *Auth) sessionInfo(s Session) map[string]any {
	return map[string]any{
		"authenticated":          true,
		"csrf":                   s.CSRF,
		"remember":               s.Remember,
		"expires_at":             s.Expires.Unix(),
		"using_default_password": a.Pass.PanelPasswordIsDefault(),
	}
}

func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
		Remember *bool  `json:"remember"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	key := a.Proxies.ClientKey(r)
	if wait := a.checkRate(key); wait > 0 {
		secs := int(wait.Seconds()) + 1
		w.Header().Set("Retry-After", fmt.Sprint(secs))
		writeErr(w, http.StatusTooManyRequests, "rate_limited", fmt.Sprintf("too many login attempts, please wait %ds", secs))
		return
	}
	if !a.Pass.VerifyPanelPassword(req.Password) {
		a.noteFail(key)
		d := a.FailDelay
		if d == 0 {
			d = 500 * time.Millisecond
		}
		select { // slow brute force down without holding a lock
		case <-time.After(d):
		case <-r.Context().Done():
		}
		writeErr(w, http.StatusUnauthorized, "invalid_password", "invalid panel password")
		return
	}
	a.clearFails(key)
	remember := req.Remember == nil || *req.Remember
	token, sess, err := a.Store.Create(remember, r.UserAgent(), PeerIPOrClient(a.Proxies, r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "session_store", "could not persist the session")
		return
	}
	a.setCookie(w, r, token, sess)
	writeJSON(w, http.StatusOK, a.sessionInfo(sess))
}

// PeerIPOrClient is the address shown in the device list.
func PeerIPOrClient(t TrustedProxies, r *http.Request) string { return t.ClientKey(r) }

func (a *Auth) status(w http.ResponseWriter, r *http.Request) {
	token, viaCookie := tokenOf(r)
	sess, touched, ok := a.Store.LookupTouch(token)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"authenticated":          false,
			"using_default_password": a.Pass.PanelPasswordIsDefault(),
		})
		return
	}
	if touched && viaCookie {
		a.setCookie(w, r, token, sess)
	}
	writeJSON(w, http.StatusOK, a.sessionInfo(sess))
}

func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	token, viaCookie := tokenOf(r)
	if sess, ok := a.Store.Lookup(token); ok {
		// A cookie-authenticated logout needs the CSRF token, otherwise any site
		// could log the admin out.
		if viaCookie && !csrfEqual(sess.CSRF, r.Header.Get(CSRFHeader)) {
			writeErr(w, http.StatusForbidden, "csrf", "missing or invalid "+CSRFHeader)
			return
		}
		a.Store.Revoke(token)
	}
	a.clearCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Auth) changePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !a.Pass.VerifyPanelPassword(req.Current) {
		writeErr(w, http.StatusUnauthorized, "invalid_password", "current password is wrong")
		return
	}
	if len(req.New) < minPasswordLen {
		writeErr(w, http.StatusBadRequest, "weak_password", fmt.Sprintf("new password must be at least %d characters", minPasswordLen))
		return
	}
	if a.RequireChange && req.New == "admin" {
		writeErr(w, http.StatusBadRequest, "default_password", "choose a password other than the default")
		return
	}
	if err := a.Pass.SetPanelPassword(req.New); err != nil {
		writeErr(w, http.StatusInternalServerError, "store", "could not save the password")
		return
	}
	token, _ := tokenOf(r)
	revoked := a.Store.RevokeAllExcept(token) // other devices must log in again
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "revoked_other_sessions": revoked})
}

func (a *Auth) devices(w http.ResponseWriter, r *http.Request) {
	cur, _ := SessionFrom(r.Context())
	type dev struct {
		ID       string `json:"id"`
		Created  int64  `json:"created"`
		LastSeen int64  `json:"last_seen"`
		Expires  int64  `json:"expires_at"`
		Remember bool   `json:"remember"`
		UA       string `json:"user_agent"`
		IP       string `json:"ip"`
		Current  bool   `json:"current"`
	}
	var out []dev
	for _, s := range a.Store.List() {
		out = append(out, dev{s.Short(), s.Created.Unix(), s.LastSeen.Unix(), s.Expires.Unix(), s.Remember, s.UserAgent, s.IP, s.ID == cur.ID})
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": out})
}

func (a *Auth) revokeDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cur, _ := SessionFrom(r.Context())
	if len(id) < 8 || strings.HasPrefix(cur.ID, id) {
		writeErr(w, http.StatusBadRequest, "current_session", "use logout to end the current session")
		return
	}
	if !a.Store.RevokeByShort(id) {
		writeErr(w, http.StatusNotFound, "not_found", "no such device")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Auth) revokeOthers(w http.ResponseWriter, r *http.Request) {
	token, _ := tokenOf(r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "revoked": a.Store.RevokeAllExcept(token)})
}

// allowedWhileDefault are reachable even when the default password still has to
// be changed.
func allowedWhileDefault(path string) bool {
	return strings.HasSuffix(path, "/session") || strings.HasSuffix(path, "/session/password") ||
		strings.HasSuffix(path, "/session/logout")
}

// Require protects a handler: a valid session (cookie, or the legacy header),
// a CSRF token on state-changing cookie requests, JSON bodies, and - when
// RequireChange is set - a non-default password.
func (a *Auth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, viaCookie := tokenOf(r)
		sess, touched, ok := a.Store.LookupTouch(token)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "login_required", "panel login required")
			return
		}
		safe := r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions
		if !safe {
			if viaCookie && !csrfEqual(sess.CSRF, r.Header.Get(CSRFHeader)) {
				writeErr(w, http.StatusForbidden, "csrf", "missing or invalid "+CSRFHeader)
				return
			}
			if r.ContentLength != 0 && !isJSON(r) {
				writeErr(w, http.StatusUnsupportedMediaType, "json_required", "Content-Type must be application/json")
				return
			}
		}
		if a.RequireChange && a.Pass.PanelPasswordIsDefault() && !allowedWhileDefault(r.URL.Path) {
			writeErr(w, http.StatusForbidden, "password_change_required", "change the default panel password first")
			return
		}
		if touched && viaCookie {
			a.setCookie(w, r, token, sess)
		}
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, sess)))
	})
}
