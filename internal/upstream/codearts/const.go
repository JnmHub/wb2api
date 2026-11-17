// Package codearts implements the Huawei Cloud CodeArts Agent upstream
// ("盘古 / 码道"): the OpenAI-compatible chat endpoint on snap-access, the
// Huawei Cloud SDK-HMAC-SHA256 request signature, and the OAuth2 + DPoP token
// refresh chain that keeps an account's STS credential alive.
//
// It is a port of the reverse-engineered client in codexarts (module
// codearts2api), reduced to the pieces wb2api needs: one account is one entry
// in the shared pool, so model discovery, scheduling and usage accounting stay
// in wb2api.
package codearts

const (
	// ChatPath is the OpenAI-compatible chat endpoint on snap-access.
	ChatPath = "/api/v2/chat/completions"
	// STSHost hosts the OAuth2 token endpoint (code exchange and refresh).
	// It is NOT the snap-access host: posting there answers
	// APIG.0101 "The API does not exist or has not been published".
	STSHost = "https://sts.cn-north-4.myhuaweicloud.com"
	// TokenPath exchanges an authorization code or refresh token for STS
	// credentials.
	TokenPath = "/v1/oauth2/tokens"

	// ClientID is the OAuth client of the official CodeArts Agent extension.
	// A refresh token is bound to it: refreshing with another client id is
	// rejected by STS ("invalid client id").
	ClientID = "codearts-agent"

	// HeaderMaasType routes a request to the free "benefit" quota; the value
	// must be present before the request is signed (it becomes a SignedHeader).
	HeaderMaasType = "maas_type"
	// MaasBenefit is the benefit-route header value.
	MaasBenefit = "benefit"

	// ExtraKey is the accounts.Record.Extra key holding the AK/SK chain.
	ExtraKey = "codearts"
)
