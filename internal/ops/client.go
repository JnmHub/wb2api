// Package ops holds the account operations that talk to the vendor's account
// and billing services: credit balances, the CN daily check-in and activity
// report, the INTL daily chat, the OAuth device login, profile sync, and
// account import / export. They are off the request path — run by the
// scheduler or by an operator's click.
package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"wb2api/internal/activity"
	"wb2api/internal/fingerprint"
	"wb2api/internal/jsonx"
	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
	"wb2api/internal/store/settings"
	"wb2api/internal/upstream"
	"wb2api/internal/upstream/identity"
)

// Tokens refreshes one account's credentials.
type Tokens interface {
	Refresh(ctx context.Context, a *pool.Account) error
}

// Client performs account operations.
type Client struct {
	Pool      *pool.Pool
	Up        *upstream.Manager
	Refresher Tokens
	Settings  *settings.Store
	Activity  *activity.Log
	Log       *slog.Logger
	// Persist stores a changed account record.
	Persist func(*accounts.Record) error
	// Remove deletes an account's file (nil = keep the file).
	Remove func(*accounts.Record) error
	// BaseOverride replaces every vendor base URL (tests, mock).
	BaseOverride string
	Now          func() time.Time
	// Pause replaces the inter-write sleeps (tests); nil = time.Sleep.
	Pause func(time.Duration)

	loginMu sync.Mutex
	logins  map[string]loginInfo
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) logger() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

func (c *Client) chatBase(r string) string {
	if c.BaseOverride != "" {
		return strings.TrimRight(c.BaseOverride, "/")
	}
	return realm.Get(r).ChatUpstream
}

func (c *Client) billingBase(r string) string {
	if c.BaseOverride != "" {
		return strings.TrimRight(c.BaseOverride, "/")
	}
	return realm.Get(r).BillingUpstream
}

func billingUA(r string) string {
	if r == realm.CN {
		return "WorkBuddy/" + identity.DesktopVersionCN
	}
	return "WorkBuddy/" + identity.DesktopVersionIntl
}

func lang(r string) string {
	if r == realm.CN {
		return "zh-CN"
	}
	return "en-US"
}

// billingHeaders is the lightweight header set the billing endpoints expect —
// not the desktop chat identity. Machine and session ids are per account: the
// gateway is one process posing as many devices, and a process-wide id would
// be a key that links them all.
func (c *Client) billingHeaders(rec *accounts.Record) identity.Headers {
	cfg := realm.Get(rec.Realm)
	domain := rec.Domain
	if domain == "" {
		domain = cfg.Domain
	}
	h := identity.Headers{
		identity.P("Content-Type", "application/json"),
		identity.P("Accept", "application/json, text/plain, */*"),
		identity.P("X-Requested-With", "XMLHttpRequest"),
		identity.P("User-Agent", billingUA(rec.Realm)),
		identity.P("Origin", cfg.Origin),
		identity.P("Referer", cfg.Origin+"/"),
		identity.P("Authorization", "Bearer "+rec.AccessToken),
		identity.P("X-User-Id", rec.UID),
		identity.P("X-Domain", domain),
		identity.P("X-CodeBuddy-Request", "1"),
		identity.P("Accept-Language", lang(rec.Realm)),
		identity.P("X-Request-ID", fingerprint.NewRequestID()),
		identity.P("X-Machine-ID", fingerprint.MachineID(rec.UID)),
		identity.P("X-Session-ID", fingerprint.SessionID(rec.UID)),
	}
	if eid := enterpriseID(rec); eid != "" {
		h = append(h, identity.P("X-Enterprise-Id", eid), identity.P("X-Tenant-Id", eid))
	} else {
		h = append(h, identity.P("X-No-Enterprise-Id", "1"))
	}
	if rec.Realm == realm.CN {
		h = append(h, identity.P("X-Product", "SaaS"))
		if dt := c.Settings.ResolveDeviceToken(); dt != "" && identity.NormalizeProduct(rec.Product) != identity.ProductCLI {
			h = append(h, identity.P("X-Device-Token", dt))
		}
	}
	return h
}

func enterpriseID(rec *accounts.Record) string {
	if rec.EnterpriseID != "" {
		return rec.EnterpriseID
	}
	return realm.JWTEnterpriseID(rec.AccessToken)
}

// IsEnterprise: members of an enterprise (team) space have no personal resource
// packages; their credit lives on a different endpoint.
func IsEnterprise(rec *accounts.Record) bool { return enterpriseID(rec) != "" }

// chatHeaders are the desktop-identity headers of the account's own product.
func (c *Client) chatHeaders(rec *accounts.Record) identity.Headers {
	return identity.ChatHeaders(identity.ChatParams{
		Params: identity.Params{
			Product: identity.NormalizeProduct(rec.Product), Realm: rec.Realm, UID: rec.UID, Token: rec.AccessToken,
			EnterpriseID: rec.EnterpriseID, TenantID: rec.EnterpriseID,
		},
		DeviceToken: c.Settings.ResolveDeviceToken(),
	})
}

// HTTPError is a non-2xx answer.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, strings.TrimSpace(truncate(e.Body, 200)))
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func retryable(err error) bool {
	if err == nil {
		return false
	}
	if he, ok := err.(*HTTPError); ok {
		return he.Status >= 500 || he.Status == 408 || he.Status == 429
	}
	t := strings.ToLower(err.Error())
	for _, m := range []string{"eof", "reset", "timeout", "timed out", "tls", "ssl", "broken pipe", "refused", "unreachable"} {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}

// doJSON sends one request and decodes a JSON object answer, retrying
// transport blips (CDN edges routinely drop a handshake; one retry nearly
// always works).
func (c *Client) doJSON(ctx context.Context, acc *pool.Account, method, url string, h identity.Headers, body []byte, timeout time.Duration, attempts int) (*jsonx.Obj, error) {
	if attempts < 1 {
		attempts = 1
	}
	var last error
	for i := 1; i <= attempts; i++ {
		rctx, cancel := context.WithTimeout(ctx, timeout)
		resp, err := c.Up.Wire(acc.UID, acc.Proxy()).Do(rctx, upstream.Request{Method: method, URL: url, Headers: h, Body: body})
		if err == nil {
			raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			resp.Body.Close()
			cancel()
			switch {
			case rerr != nil:
				err = rerr
			case resp.StatusCode/100 != 2:
				err = &HTTPError{Status: resp.StatusCode, Body: string(raw)}
			default:
				o, perr := jsonx.UnmarshalObj(bytes.TrimSpace(raw))
				if perr != nil {
					return nil, fmt.Errorf("unexpected response: %w", perr)
				}
				return o, nil
			}
		} else {
			cancel()
		}
		last = err
		if i >= attempts || !retryable(err) || ctx.Err() != nil {
			break
		}
		select {
		case <-time.After(time.Duration(i) * time.Second):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, last
}

func isAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	if he, ok := err.(*HTTPError); ok && he.Status == http.StatusUnauthorized {
		return true
	}
	t := strings.ToLower(err.Error())
	return strings.Contains(t, "401") || strings.Contains(t, "unauthorized") || strings.Contains(t, "authorization")
}

// update applies a change to the account's record and stores it.
func (c *Client) update(acc *pool.Account, mutate func(*accounts.Record)) {
	rec, ok := c.Pool.Update(acc.UID, mutate)
	if !ok {
		return
	}
	if c.Persist != nil {
		if err := c.Persist(rec); err != nil {
			c.logger().Warn("account not persisted", "account", short(acc.UID), "err", err)
		}
	}
}

func short(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

func (c *Client) record(acc *pool.Account, task, trigger string, ok bool, msg string) {
	if c.Activity == nil {
		return
	}
	rec := acc.Record()
	c.Activity.Record(activity.Row{UID: rec.UID, Nickname: rec.Nickname, Realm: rec.Realm, Task: task, Trigger: trigger, OK: ok, Message: msg})
}

// Result is the outcome of one operation, shaped for the panel.
type Result struct {
	OK      bool           `json:"ok"`
	Msg     string         `json:"msg,omitempty"`
	Error   string         `json:"error,omitempty"`
	Code    any            `json:"code,omitempty"`
	Extra   map[string]any `json:"extra,omitempty"`
	Relogin bool           `json:"need_relogin,omitempty"`
}

func failure(format string, a ...any) Result { return Result{Error: fmt.Sprintf(format, a...)} }

type accountsRecord = accounts.Record

func jsonNumber(n int64) json.Number { return json.Number(strconv.FormatInt(n, 10)) }
