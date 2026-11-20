package raccoon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wb2api/internal/upstream/identity"
)

// DefaultTimeout bounds the control-plane calls (auth, catalogue, balance).
// A chat turn is bounded by the gateway instead.
const DefaultTimeout = 30 * time.Second

// Client speaks the raccoon JSON envelope: {code, message, data}. Every control
// endpoint answers it, so parsing lives here instead of in each file.
type Client struct {
	// HTTP is the transport; nil means a client with DefaultTimeout.
	HTTP *http.Client
	// LlmBase/AuthBase/Origin override the package defaults (tests, staging).
	LlmBase  string
	AuthBase string
	Origin   string
}

// New builds a client with the configured bases.
func New(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}
	return &Client{HTTP: httpClient, LlmBase: LlmBase(), AuthBase: AuthBase(), Origin: Origin()}
}

func (c *Client) llmBase() string {
	if c != nil && c.LlmBase != "" {
		return strings.TrimRight(c.LlmBase, "/")
	}
	return LlmBase()
}

func (c *Client) authBase() string {
	if c != nil && c.AuthBase != "" {
		return strings.TrimRight(c.AuthBase, "/")
	}
	return AuthBase()
}

func (c *Client) origin() string {
	if c != nil && c.Origin != "" {
		return strings.TrimRight(c.Origin, "/")
	}
	return Origin()
}

func (c *Client) http() *http.Client {
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: DefaultTimeout}
}

// Response is one decoded envelope. Code is nil when the body carried no
// business code at all (an HTTP-level failure, an empty body, a non-JSON error
// page), which is different from code 0.
type Response struct {
	Status     int
	Code       *int
	Message    string
	Data       map[string]any
	Raw        map[string]any
	Bytes      []byte
	RetryAfter string
}

// OK reports an HTTP 2xx with business code 0 (or no code at all on a 2xx).
func (r *Response) OK() bool {
	if r == nil {
		return false
	}
	if r.Status < 200 || r.Status > 299 {
		return false
	}
	return r.Code == nil || *r.Code == CodeOK
}

// TokenExpired reports the upstream's three spellings of "the token is dead":
// HTTP 401, business code 200003/200009, or an authorization_verify_error text.
func (r *Response) TokenExpired() bool {
	if r == nil {
		return false
	}
	if r.Status == http.StatusUnauthorized {
		return true
	}
	if r.Code != nil && (*r.Code == CodeTokenExpired || *r.Code == CodeTokenExpiredAlt) {
		return true
	}
	lowered := strings.ToLower(r.Message + " " + r.codeText())
	return strings.Contains(lowered, "authorization_verify_error")
}

func (r *Response) codeText() string {
	if r == nil {
		return ""
	}
	for _, v := range []any{r.Raw["code"], pathOf(r.Raw, "error", "code")} {
		switch t := v.(type) {
		case string:
			return t
		case json.Number:
			return t.String()
		case float64:
			return strconv.FormatInt(int64(t), 10)
		}
	}
	return ""
}

// Do performs one request and decodes the envelope. A transport error is
// returned as-is; an HTTP error status is NOT an error -- callers decide
// (the same body shape carries 401s and quota failures).
func (c *Client) Do(ctx context.Context, method, url string, headers []identity.Pair, body any) (*Response, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("小浣熊请求体编码失败: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	for _, h := range headers {
		req.Header.Set(h.Name, h.Value)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	out := &Response{Status: resp.StatusCode, Bytes: raw, RetryAfter: resp.Header.Get("Retry-After")}
	obj := map[string]any{}
	if err := json.Unmarshal(raw, &obj); err == nil && obj != nil {
		out.Raw = obj
		out.Message = envelopeMessage(obj)
		if code, ok := businessCode(obj); ok {
			out.Code = &code
		}
		if data, ok := obj["data"].(map[string]any); ok {
			out.Data = data
		} else {
			out.Data = obj
		}
	}
	if out.Message == "" && len(raw) > 0 && out.Raw == nil {
		out.Message = strings.TrimSpace(string(raw))
	}
	if out.Message == "" {
		out.Message = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return out, nil
}

// businessCode reads body.code, then body.error.code; numbers and numeric
// strings are both accepted (the upstream is inconsistent between endpoints).
func businessCode(body map[string]any) (int, bool) {
	for _, v := range []any{body["code"], pathOf(body, "error", "code")} {
		if v == nil {
			continue
		}
		switch t := v.(type) {
		case json.Number:
			if i, err := t.Int64(); err == nil {
				return int(i), true
			}
		case float64:
			return int(t), true
		case int:
			return t, true
		case string:
			if i, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
				return i, true
			}
		}
	}
	return 0, false
}

// envelopeMessage picks the first non-empty text: message, msg, error.message,
// error (as a string) -- the four spellings observed upstream.
func envelopeMessage(body map[string]any) string {
	if s, _ := body["message"].(string); strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	if s, _ := body["msg"].(string); strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	if errObj, ok := body["error"].(map[string]any); ok {
		if s, _ := errObj["message"].(string); strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	if s, _ := body["error"].(string); strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	return ""
}

// pathOf walks nested maps without panicking on a wrong shape.
func pathOf(body map[string]any, keys ...string) any {
	var cur any = body
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

// authHeaders are the shared identity of every authed control call: bearer,
// optional organisation code and the desktop client triple. The chat path adds
// a per-request id on top; the refresh and code-exchange calls deliberately
// send none of this (see their callers).
func authHeaders(token, org string) identity.Headers {
	h := identity.Headers{}
	h.Set("Authorization", "Bearer "+StripBearer(token))
	if org != "" {
		h.Set("X-Org-Code", org)
	}
	for _, p := range clientHeaders("desktop") {
		h.Set(p.Name, p.Value)
	}
	return h
}

// clientHeaders is the official client's identity triple. platform is
// "desktop" (the default) or "mobile" for the phone-side sign-up grant.
func clientHeaders(platform string) identity.Headers {
	switch platform {
	case "mobile":
		return identity.Headers{
			{Name: "X-Client-Name", Value: ClientName},
			{Name: "X-Client-Platform", Value: "app-android"},
			{Name: "X-Client-Version", Value: MobileClientVersion},
		}
	}
	return identity.Headers{
		{Name: "X-Client-Name", Value: ClientName},
		{Name: "X-Client-Platform", Value: clientPlatform()},
		{Name: "X-Client-Version", Value: ClientVersion},
	}
}
