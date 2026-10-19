// Package identity builds the outbound request identity: the ordered header
// set that makes a gateway request look like the official WorkBuddy desktop
// client, the official VSCode extension or the official CodeBuddy CLI. It is a
// port of legacy wb_identity.py and the chat branch of Account.headers(); the
// header ORDER and spelling are part of the contract and are verified against
// the legacy output.
package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"wb2api/internal/realm"
)

// Products (outbound identities).
const (
	ProductDesktop = "workbuddy"
	ProductVSCode  = "vscode"
	ProductCLI     = "cli"
)

// Version constants (see legacy wb_identity.py for provenance).
const (
	DesktopVersionIntl = "5.8.4"
	DesktopUAIntl      = "WorkBuddy/5.8.4 workbuddy-ai/5.8.4 CLI/2.161.0"
	DesktopVersionCN   = "5.5.6"
	DesktopUACN        = "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1"

	DefaultCLIVersion    = "2.168.2"
	CLIAgentRuntime      = "other"
	RequestContentEnc    = "gzip"
	CLICacheRetention    = "long"
	VSCodeProductVersion = "4.12.39997873"
	VSCodeIDEVersion     = "1.119.0"
	VSCodePlatform       = "VSCode"
	VSCodeProductName    = "CodeBuddy"
	VSCodeEnvID          = "production"
)

// CLIBodyDropFields are request-body fields the official CLI never sends.
var CLIBodyDropFields = []string{"metadata", "conversation_id", "previous_response_id"}

// Pair is one header (name keeps its exact spelling).
type Pair struct{ Name, Value string }

// Headers is an ordered header list with Python-dict update semantics: setting
// an existing name (case-sensitive, as dict keys are) keeps its position.
type Headers []Pair

// Set replaces the value of name in place, or appends it.
func (h *Headers) Set(name, value string) {
	for i := range *h {
		if (*h)[i].Name == name {
			(*h)[i].Value = value
			return
		}
	}
	*h = append(*h, Pair{name, value})
}

// Get returns the first value for name (case-insensitive) and whether it exists.
func (h Headers) Get(name string) (string, bool) {
	for _, p := range h {
		if strings.EqualFold(p.Name, name) {
			return p.Value, true
		}
	}
	return "", false
}

// Update applies o over h (dict.update).
func (h *Headers) Update(o Headers) {
	for _, p := range o {
		h.Set(p.Name, p.Value)
	}
}

// NormalizeProduct accepts the legacy spellings.
func NormalizeProduct(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "workbuddy", "wb", "desktop":
		return ProductDesktop
	case "vscode", "vsc", "ide":
		return ProductVSCode
	case "cli", "codebuddy":
		return ProductCLI
	}
	return ProductDesktop
}

// Endpoint returns the chat base URL and the X-Domain value for a realm and
// product.
func Endpoint(r, product string) (base, domain string) {
	product = NormalizeProduct(product)
	if r == realm.CN {
		cnBase, _ := realm.CNChatBase()
		if product == ProductVSCode {
			return "https://www.workbuddy.cn", "www.workbuddy.cn"
		}
		return cnBase, "copilot.tencent.com"
	}
	return "https://www.workbuddy.ai", "www.workbuddy.ai"
}

// ------------------------------------------------------------------ ids

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// UUID7 returns an RFC 9562 UUIDv7 (48-bit ms timestamp, version 7, variant 10).
func UUID7() [16]byte {
	var u [16]byte
	ms := uint64(time.Now().UnixMilli()) & (1<<48 - 1)
	r := randBytes(10)
	// bytes 0..5: timestamp
	u[0], u[1], u[2], u[3], u[4], u[5] = byte(ms>>40), byte(ms>>32), byte(ms>>24), byte(ms>>16), byte(ms>>8), byte(ms)
	copy(u[6:], r)
	u[6] = 0x70 | (u[6] & 0x0f) // version 7
	u[8] = 0x80 | (u[8] & 0x3f) // variant 10
	return u
}

func uuidString(u [16]byte) string {
	h := hex.EncodeToString(u[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// NewConversationID is a hyphenated UUIDv7.
func NewConversationID() string { return uuidString(UUID7()) }

// NewMessageID is a 32-char hex UUIDv7 (X-Request-ID shape).
func NewMessageID() string { u := UUID7(); return hex.EncodeToString(u[:]) }

// NewTurnRequestID is generated once per user turn, outside any retry loop.
func NewTurnRequestID() string { return NewMessageID() }

// NewRequestTraceID is a random hyphenated UUIDv4 (X-Request-Trace-Id).
func NewRequestTraceID() string {
	u := [16]byte(randBytes(16))
	u[6] = 0x40 | (u[6] & 0x0f)
	u[8] = 0x80 | (u[8] & 0x3f)
	return uuidString(u)
}

// ConversationIDFor derives a stable conversation id from (uid, session key);
// empty when there is no session key.
func ConversationIDFor(uid, sessionKey string) string {
	key := strings.TrimSpace(sessionKey)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(uid + "|" + key))
	h := []byte(hex.EncodeToString(sum[:])[:32])
	h[12], h[16] = '4', 'a'
	s := string(h)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

func hex16() string { return hex.EncodeToString(randBytes(8)) }

// ------------------------------------------------------------ header families

func traceHeaders(withParent bool) Headers {
	trace, span := NewMessageID(), hex16()
	h := Headers{
		{"X-Trace-ID", trace},
		{"traceparent", fmt.Sprintf("00-%s-%s-01", trace, span)},
		{"b3", fmt.Sprintf("%s-%s-1", trace, span)},
		{"X-B3-TraceId", trace},
		{"X-B3-SpanId", span},
		{"X-B3-Sampled", "1"},
	}
	if withParent {
		parent := hex16()
		h.Set("X-B3-ParentSpanId", parent)
		h.Set("b3", fmt.Sprintf("%s-%s-1-%s", trace, span, parent))
	}
	return h
}

func vscodeTraceHeaders() Headers {
	trace, span, parent := NewMessageID(), hex16(), hex16()
	return Headers{
		{"X-Trace-ID", trace},
		{"b3", fmt.Sprintf("%s-%s-1-%s", trace, span, parent)},
		{"X-B3-TraceId", trace},
		{"X-B3-ParentSpanId", parent},
		{"X-B3-SpanId", span},
		{"X-B3-Sampled", "1"},
	}
}

func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

func identityOS() string {
	if v := strings.TrimSpace(os.Getenv("WB_IDENTITY_OS")); v != "" {
		return v
	}
	switch runtime.GOOS {
	case "windows":
		return "Windows"
	case "darwin":
		return "MacOS"
	case "linux":
		return "Linux"
	}
	return runtime.GOOS
}

func identityArch() string {
	if v := strings.TrimSpace(os.Getenv("WB_IDENTITY_ARCH")); v != "" {
		return v
	}
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	case "386":
		return "ia32"
	}
	return runtime.GOARCH
}

func stainlessHeaders() Headers {
	return Headers{
		{"x-stainless-lang", "js"},
		{"x-stainless-package-version", "6.25.0"},
		{"x-stainless-os", identityOS()},
		{"x-stainless-arch", identityArch()},
		{"x-stainless-runtime", "node"},
		{"x-stainless-runtime-version", envOr("WB_IDENTITY_NODE_VERSION", "v22.21.1")},
		{"x-stainless-retry-count", "0"},
		{"x-stainless-timeout", "1200"},
	}
}

// Params are the inputs of BuildHeaders.
type Params struct {
	Product        string
	Realm          string
	UID            string
	Token          string
	ConversationID string // explicit; empty = derive/generate per product
	EnterpriseID   string
	TenantID       string
	Department     string
	Model          string
	SessionKey     string
	TurnRequestID  string
}

func vscodeIDEVersion() string { return envOr("WB_VSCODE_IDE_VERSION", VSCodeIDEVersion) }

func nz(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// BuildHeaders returns the identity header set in the legacy insertion order.
func BuildHeaders(p Params) Headers {
	product := NormalizeProduct(p.Product)
	isCN := p.Realm == realm.CN
	_, domain := Endpoint(p.Realm, product)

	conv := strings.TrimSpace(p.ConversationID)
	explicit := conv != ""
	if !explicit {
		if product == ProductVSCode {
			conv = NewMessageID()
		} else {
			conv = NewConversationID()
		}
	}
	msgID := NewMessageID()
	convReqID := NewMessageID()
	if product == ProductCLI {
		if !explicit {
			if d := ConversationIDFor(p.UID, p.SessionKey); d != "" {
				conv = d
			}
		}
		convReqID = nz(strings.TrimSpace(p.TurnRequestID), NewMessageID())
	}
	uid := nz(p.UID, "anonymous")

	var h Headers
	switch product {
	case ProductDesktop:
		ver, ua := DesktopVersionIntl, DesktopUAIntl
		if isCN {
			ver, ua = DesktopVersionCN, DesktopUACN
		}
		h = Headers{
			{"X-Agent-Intent", "craft"}, {"X-Agent-Purpose", "conversation"}, {"X-Agent-Type", "main"},
			{"X-IDE-Name", "WorkBuddy"}, {"X-IDE-Type", "WorkBuddy"}, {"X-IDE-Version", ver},
			{"X-Private-Data", "true"}, {"X-Product", "SaaS"}, {"X-Domain", domain},
			{"User-Agent", ua}, {"X-User-Id", uid}, {"Authorization", "Bearer " + p.Token},
			{"X-Conversation-ID", conv}, {"X-Conversation-Request-ID", convReqID},
			{"X-Root-Request-ID", convReqID}, {"X-Conversation-Message-ID", msgID},
			{"X-Request-ID", msgID},
		}
		h.Update(traceHeaders(false))
	case ProductVSCode:
		ide := vscodeIDEVersion()
		h = Headers{
			{"X-Agent-Intent", "craft"}, {"X-Agent-Type", "main"},
			{"X-IDE-Type", VSCodePlatform}, {"X-IDE-Name", VSCodePlatform}, {"X-IDE-Version", ide},
			{"X-Product-Version", VSCodeProductVersion}, {"X-Env-ID", VSCodeEnvID},
			{"X-Product", "SaaS"}, {"X-Domain", domain},
			{"User-Agent", fmt.Sprintf("%s/%s %s/%s", VSCodePlatform, ide, VSCodeProductName, VSCodeProductVersion)},
			{"X-User-Id", uid}, {"Authorization", "Bearer " + p.Token},
			{"X-Request-Trace-Id", NewRequestTraceID()},
			{"X-Conversation-ID", conv}, {"X-Conversation-Request-ID", convReqID},
			{"X-Conversation-Message-ID", msgID}, {"X-Request-ID", convReqID},
		}
		if p.Model != "" {
			now := fmt.Sprint(time.Now().UnixMilli())
			h.Set("X-Model-ID", p.Model)
			h.Set("monitor_promptPrepareStartTime", now)
			h.Set("monitor_httpSendTime", now)
		}
		if !isCN {
			h.Set("X-Private-Data", "true")
		}
		h.Update(vscodeTraceHeaders())
	default: // CLI
		h = Headers{
			{"X-Agent-Intent", "craft"}, {"X-Agent-Purpose", "conversation"},
			{"X-Agent-Runtime", CLIAgentRuntime}, {"X-Agent-Type", "main"},
			{"X-IDE-Type", "CLI"}, {"X-IDE-Name", "CLI"}, {"X-IDE-Version", DefaultCLIVersion},
			{"X-Product", "SaaS"}, {"X-Domain", domain},
			{"User-Agent", fmt.Sprintf("CLI/%s CodeBuddy/%s", DefaultCLIVersion, DefaultCLIVersion)},
			{"X-User-Id", uid}, {"Authorization", "Bearer " + p.Token},
			{"X-Conversation-ID", conv}, {"X-Conversation-Request-ID", convReqID},
			{"X-Root-Request-ID", convReqID}, {"X-Conversation-Message-ID", msgID},
			{"X-Request-ID", msgID},
		}
		if p.EnterpriseID != "" {
			h.Set("X-Private-Data", "true")
		} else {
			h.Set("X-Private-Data", "false")
		}
		h.Update(stainlessHeaders())
		h.Update(traceHeaders(true))
	}
	if p.EnterpriseID != "" {
		h.Set("X-Enterprise-Id", p.EnterpriseID)
		h.Set("X-Tenant-Id", nz(p.TenantID, p.EnterpriseID))
	}
	if p.Department != "" {
		h.Set("X-Department-Info", p.Department)
	}
	return h
}

// ChatParams adds the account-level inputs of the chat header assembly.
type ChatParams struct {
	Params
	Origin      string // realm origin, e.g. https://www.workbuddy.ai
	DeviceToken string // optional literal/file token (CN, non-CLI only)
}

// ChatHeaders reproduces Account.headers(purpose="chat"): the browser-ish base
// headers (none for CLI), then the identity set, then the device token rules.
func ChatHeaders(c ChatParams) Headers {
	product := NormalizeProduct(c.Product)
	cfg := realm.Get(c.Realm)
	origin := nz(c.Origin, cfg.Origin)
	var h Headers
	if product == ProductCLI {
		h = Headers{{"Content-Type", "application/json"}, {"Accept", "application/json"}}
	} else {
		lang := "zh-CN"
		if c.Realm == realm.Intl {
			lang = "en-US"
		}
		h = Headers{
			{"Content-Type", "application/json"},
			{"Accept", "application/json, text/plain, */*"},
			{"X-Requested-With", "XMLHttpRequest"},
			{"Origin", origin},
			{"Referer", origin + "/"},
			{"X-CodeBuddy-Request", "1"},
			{"Accept-Language", lang},
		}
	}
	h.Update(BuildHeaders(c.Params))
	token := ""
	if c.Realm == realm.CN && product != ProductCLI {
		token = c.DeviceToken
	}
	if token != "" {
		h.Set("X-Device-Token", token)
	} else if product == ProductVSCode && c.EnterpriseID == "" {
		h.Set("X-Device-Token-Error", "sdk_not_initialized")
	}
	return h
}

// P builds a header pair (keyed-literal-free helper for other packages).
func P(name, value string) Pair { return Pair{Name: name, Value: value} }
