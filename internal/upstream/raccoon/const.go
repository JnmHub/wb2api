// Package raccoon implements the SenseTime 小浣熊 (raccoon) upstream: an
// OpenAI-compatible chat endpoint on xiaohuanxiong.com plus the web endpoints
// behind signing in, the model catalogue, credit balances, the daily points
// grant and the two one-off sign-up grants.
//
// It is a port of the raccoon provider in aimod-cc/Agent2Providers
// (crates/providers/src/providers/raccoon, 4866 lines of Rust), reduced to what
// wb2api needs: one account is one entry in the shared pool, so routing,
// cooldowns, key binding and usage accounting stay in wb2api.
package raccoon

import (
	"os"
	"strings"

	"wb2api/internal/realm"
)

const (
	// ChatPath is the OpenAI-compatible chat endpoint, relative to LlmBase().
	// The base already carries the /api/web/llm/v2 prefix.
	ChatPath = "/chat/completions"
	// ModelCatalogPath lists the models the account may call.
	ModelCatalogPath = "/model_catalog"
	// ImagesGenPath and ImagesEditPath are the image endpoints. wb2api has no
	// /v1/images surface yet, so only the catalogue uses their model id.
	ImagesGenPath  = "/images/gen"
	ImagesEditPath = "/images/edits"

	// RefreshPath exchanges a refresh token for a new access token.
	RefreshPath = "/refresh"
	// UserInfoPath returns the platform profile (id, name, office identity).
	UserInfoPath = "/user_info"
	// IAMConfirmPath confirms a first IAM migration; business code 200901 asks
	// for it exactly once.
	IAMConfirmPath = "/iam/confirm"
	// ExchangeCodePath trades a web-login authorization code for credentials.
	ExchangeCodePath = "/login_with_authorization_code"

	// BalancePath returns the credit wallets (five buckets).
	BalancePath = "/api/web/points/v1/balance"
	// EntitlementPath returns the subscription (pro plan) state.
	EntitlementPath = "/api/web/auth/v1/entitlement_info"
	// BillsPath is the credit ledger; the daily grant is confirmed there.
	BillsPath = "/api/web/points/v2/bills"
	// SettingInfoPersonalPath is the personal "home" endpoint. Opening it is
	// what makes the upstream grant the daily points: there is no check-in
	// verb, the grant is a per-day idempotent side effect of this call.
	SettingInfoPersonalPath = "/api/web/office/v3/setting_info"
	// SettingInfoOrgPath is the same endpoint for an organisation account.
	SettingInfoOrgPath = "/api/web/org/user/office/v1/setting_info"
	// DesktopGrantPath and MobileGrantPath are the two one-off sign-up grants.
	DesktopGrantPath = "/api/web/desktop/v1/login/points/grant"
	MobileGrantPath  = "/api/web/mobile/v1/login/points/grant"
	// AuthorizePath is the web login page.
	AuthorizePath = "/authorize"

	// LoginSourceDesktop makes the authorize page answer with a
	// office-raccoon:// redirect; LoginSourceWeb makes it append the code to
	// the redirect query instead (the only mode a server deployment can use).
	LoginSourceDesktop = "desktop"
	LoginSourceWeb     = "web"
	// AppName is the client name the official page shows; it is a Chinese
	// literal and must be percent-encoded when building the URL.
	AppName = "办公小浣熊客户端"

	// CallbackScheme/Host/Path identify the desktop callback. Every component
	// must match exactly: a prefix check would accept
	// office-raccoon://auth/callback@evil.com.
	CallbackScheme = "office-raccoon"
	CallbackHost   = "auth"
	CallbackPath   = "/callback"

	// ClientName, ClientVersion and MobileClientVersion are the client identity
	// headers the official apps send. The upstream reads them as "this is a
	// desktop client", so they are part of the contract, not decoration.
	ClientName          = "raccoon-work"
	ClientVersion       = "v1.1.5"
	MobileClientVersion = "v1.0.3"

	// ExtraKey is the accounts.Record.Extra key holding the raccoon metadata.
	ExtraKey = "raccoon"
	// DefaultModel is the idempotent default of the static catalogue.
	DefaultModel = "raccoon-405a1c"
	// ProbeModel is the model the panel's "test account" button uses when the
	// operator leaves the field empty: the cheap, always-on chat model.
	ProbeModel = "sn-deepseek-v4-1-flash-1017"
	// ImageModelID is the synthetic catalogue entry for image generation.
	ImageModelID = "raccoon-image"

	// Business codes (upstream envelope "code").
	CodeOK                 = 0
	CodeTokenExpired       = 200003
	CodeTokenExpiredAlt    = 200009
	CodeAuthCodeExpired    = 200035
	CodeIAMConfirmRequired = 200901
	CodeInsufficientPoints = 1000007
)

// Base URLs. They are overridable so an operator can point a test account at a
// staging host without rebuilding, mirroring RACCOON_LLM_BASE_URL and friends.
const (
	defaultAuthBase = "https://xiaohuanxiong.com/api/web/auth/v1"
	defaultOrigin   = "https://xiaohuanxiong.com"
)

func baseFromEnv(name, def string) string {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	return strings.TrimRight(raw, "/")
}

// LlmBase is the chat/catalogue root (WB_RACCOON_LLM_BASE).
func LlmBase() string { return realm.RaccoonLLMBase() }

// AuthBase is the auth API root (WB_RACCOON_AUTH_BASE); the refresh, profile
// and code-exchange endpoints hang off it.
func AuthBase() string { return baseFromEnv("WB_RACCOON_AUTH_BASE", defaultAuthBase) }

// Origin is the main site (WB_RACCOON_ORIGIN): balance, grants, bills and the
// authorize page.
func Origin() string { return baseFromEnv("WB_RACCOON_ORIGIN", defaultOrigin) }

// aliases maps the vendor-neutral names other channels use onto this channel's
// ids, so one client config can name the same family everywhere:
//
//	deepseek-v4.1-flash  (WorkBuddy)  ->  sn-deepseek-v4-1-flash-1017 (raccoon)
//
// Only names that actually differ need an entry; every sn-* id is already the
// wire name. Adding a line here also makes /v1/chat/completions accept the
// short name instead of refusing it as "not available on this channel".
var aliases = map[string]string{
	"deepseek-v4.1-flash":      "sn-deepseek-v4-1-flash-1017",
	"deepseek-v4.1-flash-1017": "sn-deepseek-v4-1-flash-1017",
	"kimi-k3-1-pro":            "sn-kimi-k3-1-pro",
	"qwen4-max":                "sn-qwen4-max",
	"sensenova-6-8-flash":      "sn-sensenova-6-8-flash",
	"sensenova-6-8-flash-lite": "sn-sensenova-6-8-flash-lite",
}

// Alias resolves a vendor-neutral model name to this channel's id ("" when the
// name is not an alias).
func Alias(model string) string {
	return aliases[strings.ToLower(strings.TrimSpace(model))]
}
