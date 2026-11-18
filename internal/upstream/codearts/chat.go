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

// sanitiseParams removes the combinations the upstream rejects with a bare
// InferHub.001001005 "The request param is invalid":
//
//   - max_tokens together with max_completion_tokens (two output budgets)
//   - n > 1 (only one choice is ever produced)
//   - stream_options (streaming is forced on; include_usage is our business)
//
// Everything else is forwarded untouched: the upstream may still reject it, but
// silently rewriting a value the client asked for would be worse than a clear
// error with the diagnostic record.
func sanitiseParams(out *jsonx.Obj) {
	if out.Has("max_completion_tokens") && out.Has("max_tokens") {
		out.Delete("max_completion_tokens")
	}
	if v, ok := out.Lookup("n"); ok {
		if n, ok := v.(float64); !ok || n != 1 {
			out.Delete("n")
		}
	}
	out.Delete("stream_options")
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
				// Clone before rewriting: the shaped body is shared by every
				// attempt of this request, so shortening in place would leave the
				// next attempt with already-shortened names and a stale mapping.
				if o, ok := t.(*jsonx.Obj); ok {
					co := o.Clone()
					// Clone is shallow: the function object is still shared, and
					// rewriting its name in place would leak into every later
					// attempt of the same request.
					if fn, ok := co.Obj("function"); ok {
						co.Set("function", fn.Clone())
					}
					cp[i] = co
					continue
				}
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
				// Everything else that references a tool name has to move with it.
				nm.RewriteToolChoice(out)
				nm.RewriteHistory(out)
			}
		}
	}
	sanitiseParams(out)
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
func ChatHeaders(cred Credential, token, traceID, language, sessionID, model string, benefit bool, body []byte) identity.Headers {
	if strings.TrimSpace(traceID) == "" {
		traceID = randomHex(8)
	}
	if strings.TrimSpace(language) == "" {
		language = "zh-cn"
	}
	h := identity.Headers{
		identity.P("Content-Type", "application/json"),
		identity.P("Accept", "text/event-stream"),
		identity.P("client_version", "Vscode_"+PluginVersion),
		identity.P("Agent-Type", "ChatAgent"),
		identity.P("X-Language", language),
		identity.P("is_confidential", "false"),
		identity.P("plugin-name", PluginName),
		identity.P("plugin-version", PluginVersion),
		identity.P("x-auth-token", token),
		identity.P("x-snap-traceid", traceID),
		identity.P("app-id", "CodeAgent3.0"),
	}
	if model = strings.TrimSpace(model); model != "" {
		// The upstream treats the model name as a request header as well: all
		// three keys are expected, and the value must be the wire name (the
		// catalogue id), not the display alias the client used.
		h = append(h,
			identity.P("model-id", model),
			identity.P("model-name", model),
			identity.P("x-model-id", model),
		)
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

// IsGatewayRejection reports an APIG-level refusal: the request never reached
// the model. These codes are about the account or the gateway, not the body:
//
//	TM.00001001  request body is invalid JSON (e.g. a compressed body sent raw)
//	TM.00001041  session concurrency limit (three conversations per account)
//	TM.00001050  request rejected by the gateway (observed in bursts; the body
//	             shape does not reproduce it — 200 KB bodies, 200 tools and
//	             unknown fields all pass, so it is not a request-shape problem)
//	TM.00001051  this account may not call the model
//
// Treating one of these as "the client sent something bad" would blame the
// caller for a transient account-side condition.
func IsGatewayRejection(body []byte) bool {
	s := string(body)
	return strings.Contains(s, "TM.00001001") ||
		strings.Contains(s, "TM.00001041") ||
		strings.Contains(s, "TM.00001050") ||
		strings.Contains(s, "APIG.0301") ||
		strings.Contains(s, "APIG.0602")
}
