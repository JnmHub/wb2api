package codearts

import (
	"strings"

	"wb2api/internal/jsonx"
	"wb2api/internal/realm"
	"wb2api/internal/upstream/identity"
)

// bodyKeys are the chat fields the CodeArts endpoint understands. Everything
// else the WorkBuddy shaping layer produces (metadata, conversation_id,
// previous_response_id, product markers) is dropped: the upstream validates
// its request schema strictly and answers an unknown field with
// InferHub.001001005 instead of ignoring it.
var bodyKeys = []string{
	"messages", "stream_options", "tools", "tool_choice",
	"temperature", "top_p", "max_tokens", "max_completion_tokens",
	"stop", "reasoning_effort", "presence_penalty", "frequency_penalty",
	"seed", "user", "response_format", "n",
}

// PrepareBody renders the CodeArts chat body from the already-shaped request.
//
// The upstream is stream-only in practice: the official client always asks for
// SSE and reassembles locally, so the flag is forced to true here. A client
// that asked for a non-stream response is served by wb2api's own aggregation
// over that stream, which keeps one wire shape for both modes.
func PrepareBody(shaped *jsonx.Obj, model, chatID string) ([]byte, *ToolNameMap, error) {
	out := jsonx.NewObj()
	if shaped != nil {
		for _, k := range bodyKeys {
			if v, ok := shaped.Lookup(k); ok {
				out.Set(k, v)
			}
		}
	}
	// The upstream rejects function names longer than 64 characters before it
	// even parses the request; shorten them and keep the mapping for the reply.
	var names *ToolNameMap
	if v, ok := out.Lookup("tools"); ok {
		if tools, ok := v.([]any); ok {
			cp := make([]any, len(tools))
			for i, t := range tools {
				if m, ok := t.(map[string]any); ok {
					mc := make(map[string]any, len(m))
					for k, val := range m {
						mc[k] = val
					}
					if fn, ok := mc["function"].(map[string]any); ok {
						fc := make(map[string]any, len(fn))
						for k, val := range fn {
							fc[k] = val
						}
						mc["function"] = fc
					}
					cp[i] = mc
					continue
				}
				cp[i] = t
			}
			if nm := ShortenTools(cp); nm != nil {
				out.Set("tools", cp)
				names = nm
			}
		}
	}
	out.Set("model", model)
	out.Set("stream", true)
	out.Set("tool_stream", true)
	if chatID = strings.TrimSpace(chatID); chatID != "" {
		out.Set("chat_id", chatID)
		out.Set("prompt_cache_key", chatID)
	}
	body, err := jsonx.Marshal(out)
	return body, names, err
}

// ChatHeaders builds and signs the outbound header set of one attempt.
//
// The signature covers every header in the list, so maas_type (when the model
// rides the free-benefit route) is added before signing and nothing that the
// upstream verifies may be added afterwards.
func ChatHeaders(cred Credential, token, traceID, language, sessionID string, benefit bool, body []byte) identity.Headers {
	if strings.TrimSpace(traceID) == "" {
		traceID = randomHex(8)
	}
	if strings.TrimSpace(language) == "" {
		language = "zh-cn"
	}
	h := identity.Headers{
		identity.P("Content-Type", "application/json"),
		identity.P("Accept", "text/event-stream"),
		identity.P("x-auth-token", token),
		identity.P("x-snap-traceid", traceID),
		identity.P("X-Language", language),
		identity.P("app-id", "CodeAgent3.0"),
		identity.P("is_confidential", "false"),
	}
	if strings.TrimSpace(sessionID) != "" {
		// The upstream ties a chat to the busy lease that was registered for it;
		// without this header the request counts against no session.
		h = append(h, identity.P("user-session-id", sessionID))
	}
	if benefit {
		h = append(h, identity.P(HeaderMaasType, MaasBenefit))
	}
	target := strings.TrimRight(realm.CodeArtsChatBase, "/") + ChatPath
	return Signer{}.SignHeaders(h, "POST", target, body, cred)
}

// IsFatalRequestError reports an upstream parameter rejection: the request is
// wrong, so retrying on another account (or cooling this one) would only burn
// the pool.
func IsFatalRequestError(body []byte) bool {
	s := string(body)
	return strings.Contains(s, "InferHub.001001005") || strings.Contains(s, "001001005")
}

// IsModelAccessError reports a per-account model/permission problem, which is
// an account-capability fact rather than a transport failure.
func IsModelAccessError(body []byte) bool {
	s := strings.ToLower(string(body))
	return strings.Contains(s, "inferhub.002002009") ||
		strings.Contains(s, "tm.000010051") ||
		strings.Contains(string(body), "无权调用该模型") ||
		strings.Contains(s, "benefit not found")
}
