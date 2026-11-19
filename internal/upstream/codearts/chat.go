package codearts

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
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
	"messages", "tools", "tool_choice", "parallel_tool_calls",
	"temperature", "top_p", "max_tokens", "max_completion_tokens",
	"stop", "reasoning_effort", "reasoning", "thinking", "enable_thinking",
	"presence_penalty", "frequency_penalty", "seed", "user", "response_format",
}

const maxOutputTokens = 65536

// A decimal or exponent can denote a whole token count (e.g. 10.0e-1).
// Check the decimal digits before float conversion can round away a fraction.
func wholeTokenNumber(n json.Number) bool {
	raw := string(n)
	var exponent int64
	if i := strings.IndexAny(raw, "eE"); i >= 0 {
		exponent, _ = strconv.ParseInt(raw[i+1:], 10, 64) // overflow saturates
		raw = raw[:i]
	}
	integer, fraction, _ := strings.Cut(raw, ".")
	if places := len(strings.TrimRight(fraction, "0")); places > 0 {
		return exponent >= int64(places)
	}
	trailingZeros := len(integer) - len(strings.TrimRight(integer, "0"))
	return exponent >= -int64(trailingZeros)
}

// sanitiseParams removes unsupported options, caps the selected output budget
// and returns its positive integer value for request diagnostics.
func sanitiseParams(out *jsonx.Obj) int64 {
	if effort, _ := out.Str("reasoning_effort"); strings.EqualFold(effort, "off") || strings.EqualFold(effort, "none") {
		out.Delete("reasoning_effort")
	}
	if out.Get("enable_thinking") == false {
		out.Delete("enable_thinking")
	}

	key := "max_tokens"
	if out.Has(key) {
		out.Delete("max_completion_tokens")
	} else {
		key = "max_completion_tokens"
	}
	var tokens float64
	switch v := out.Get(key).(type) {
	case json.Number:
		if strings.ContainsAny(string(v), ".eE") && !wholeTokenNumber(v) {
			return 0
		}
		// A positive overflow is +Inf, which also exceeds the limit.
		tokens, _ = v.Float64()
	case float64:
		tokens = v
	case int:
		tokens = float64(v)
	case int64:
		tokens = float64(v)
	default:
		return 0
	}
	if tokens <= 0 || tokens != math.Trunc(tokens) {
		return 0
	}
	if tokens > maxOutputTokens {
		out.Set(key, maxOutputTokens)
		return maxOutputTokens
	}
	return int64(tokens)
}

// IncludeUsage reports whether the client opted in to stream usage frames.
func IncludeUsage(payload *jsonx.Obj) bool {
	if payload == nil {
		return false
	}
	value, _ := objGet(payload.Get("stream_options"), "include_usage")
	include, _ := value.(bool)
	return include
}

// PrepareBody renders the CodeArts chat body from a Chat Completions request.
func PrepareBody(shaped *jsonx.Obj, model, chatID string) ([]byte, *ToolNameMap, error) {
	body, names, _, err := PrepareBodyWithBudget(shaped, model, chatID)
	return body, names, err
}

// PrepareBodyWithBudget also returns the positive integer output budget on the
// wire, or zero when the request omits it or specifies an invalid value.
//
// The upstream is stream-only in practice: the official client always asks for
// SSE and reassembles locally, so the flag is forced to true here. A client
// that asked for a non-stream response is served by wb2api's own aggregation
// over that stream, which keeps one wire shape for both modes.
func PrepareBodyWithBudget(shaped *jsonx.Obj, model, chatID string) ([]byte, *ToolNameMap, int64, error) {
	if shaped == nil {
		return nil, nil, 0, errors.New("messages must be an array")
	}
	if _, ok := shaped.Arr("messages"); !ok {
		return nil, nil, 0, errors.New("messages must be an array")
	}
	if !singleChatChoice(shaped.Get("n")) {
		return nil, nil, 0, errors.New("CodeArts supports only n=1")
	}
	out := jsonx.NewObj()
	for _, k := range bodyKeys {
		if v, ok := shaped.Lookup(k); ok {
			out.Set(k, v)
		}
	}
	tools, _ := out.Arr("tools")
	selected, err := selectChatTool(out, tools)
	if err != nil {
		return nil, nil, 0, err
	}
	// The upstream rejects function names longer than 64 characters before it
	// even parses the request; shorten them and keep the mapping for the reply.
	var names *ToolNameMap
	if len(tools) > 0 {
		cp := append([]any(nil), tools...)
		for i, tool := range tools {
			if name := toolFunctionName(tool); len([]rune(name)) > MaxToolNameChars {
				cp[i] = cloneWithFunction(tool)
			}
		}
		names = ShortenTools(cp)
		names.RewriteHistory(out)
		if selected >= 0 {
			// CodeArts takes a string choice; limiting the declarations preserves
			// the client's named selection when we translate it to required.
			cp = cp[selected : selected+1]
		}
		out.Set("tools", cp)
	}
	budget := sanitiseParams(out)
	out.Set("model", model)
	out.Set("stream", true)
	out.Set("tool_stream", true)
	usageOptions := jsonx.NewObj()
	usageOptions.Set("include_usage", true)
	out.Set("stream_options", usageOptions)
	if chatID = strings.TrimSpace(chatID); chatID != "" {
		out.Set("chat_id", chatID)
		out.Set("prompt_cache_key", chatID)
	}
	body, err := jsonx.Marshal(out)
	return body, names, budget, err
}

func singleChatChoice(v any) bool {
	switch n := v.(type) {
	case nil:
		return true
	case json.Number:
		return n == "1"
	case int:
		return n == 1
	case int64:
		return n == 1
	case float64:
		return n == 1
	default:
		return false
	}
}

func selectChatTool(out *jsonx.Obj, tools []any) (int, error) {
	var name string
	switch choice := out.Get("tool_choice").(type) {
	case nil:
		out.Delete("tool_choice")
		return -1, nil
	case string:
		switch choice {
		case "auto", "none":
			return -1, nil
		case "required":
			if len(tools) == 0 {
				return -1, errors.New("tool_choice required needs at least one tool")
			}
			return -1, nil
		}
		name = choice
	case *jsonx.Obj, map[string]any:
		typ, _ := objStr(choice, "type")
		if typ == "function" {
			fn, _ := objGet(choice, "function")
			name, _ = objStr(fn, "name")
			if name == "" { // Responses uses a flat name on a named choice.
				name, _ = objStr(choice, "name")
			}
		}
	}
	if name == "" {
		return -1, errors.New("invalid tool_choice")
	}
	for i, tool := range tools {
		if toolFunctionName(tool) == name {
			out.Set("tool_choice", "required")
			return i, nil
		}
	}
	return -1, errors.New("tool_choice references an undeclared tool")
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
