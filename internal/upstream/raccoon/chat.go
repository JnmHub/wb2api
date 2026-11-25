package raccoon

import (
	"encoding/json"
	"strings"

	"wb2api/internal/jsonx"
	"wb2api/internal/upstream/chathistory"
	"wb2api/internal/upstream/identity"
)

// reasoningField aliases the four spellings clients use for the thinking knob.
var reasoningFields = []string{"reasoning_effort", "reasoningEffort", "reasoning", "thinking", "enable_thinking"}

// modelName normalises a model id for family detection: the last path segment,
// trimmed and lowercased (clients may send "raccoon/sn-glm-5-3").
func modelName(model string) string {
	if i := strings.LastIndexByte(model, '/'); i >= 0 {
		model = model[i+1:]
	}
	return strings.ToLower(strings.TrimSpace(model))
}

// family classifies a model into the upstream's thinking-parameter families.
//
//	thinking  sn-deepseek-* / deepseek-*   -> thinking{type}
//	low_high  sn-glm-5-3 / sn-kimi-k3      -> reasoning_effort low|high
//	effort    sn-sensenova-* / raccoon-*   -> reasoning_effort low|medium|high
//
// Other models have no confirmed thinking parameter and are left untouched.
func family(model string) string {
	m := modelName(model)
	switch {
	case strings.HasPrefix(m, "sn-deepseek-"), strings.HasPrefix(m, "deepseek-"):
		return "thinking"
	case strings.HasPrefix(m, "sn-glm-5-3"), strings.HasPrefix(m, "sn-kimi-k3"):
		return "low_high"
	case strings.HasPrefix(m, "sn-sensenova-"), strings.HasPrefix(m, "raccoon-"):
		return "effort"
	}
	return ""
}

// SupportsThinking reports whether the model has a confirmed thinking knob.
func SupportsThinking(model string) bool { return family(model) != "" }

// effortRank orders the accepted effort levels; ok=false means "off/unknown".
func effortRank(level string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "minimal":
		return 0, true
	case "low":
		return 1, true
	case "medium":
		return 2, true
	case "high":
		return 3, true
	case "xhigh":
		return 4, true
	case "max":
		return 5, true
	}
	return 0, false
}

func isThinkingOff(level string) bool {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "off", "none", "disabled":
		return true
	}
	return false
}

// NormalizeBody adjusts only the thinking fields of a chat body and returns a
// NEW object: the caller's payload is never mutated.
//
// Everything else is passed through untouched. Raccoon's endpoint is plain
// OpenAI, and the WorkBuddy shaping layer (system prompt, tool renaming,
// parameter whitelist) must NOT run for this channel: it would inject a
// prompt the upstream never asked for.
func NormalizeBody(payload *jsonx.Obj, model string) *jsonx.Obj {
	if payload == nil {
		return nil
	}
	out := payload.Clone()
	// accept the camelCase spelling as an alias
	if !out.Has("reasoning_effort") {
		if v, ok := out.Lookup("reasoningEffort"); ok && v != nil {
			out.Delete("reasoningEffort")
			out.Set("reasoning_effort", v)
		}
	}
	if model == "" {
		if m, ok := out.Str("model"); ok {
			model = m
		}
	}
	effort, _ := out.Str("reasoning_effort")
	switch family(model) {
	case "thinking":
		// The upstream accepts named levels verbatim on this family; only
		// "off" has to be rewritten, because it is spelled as the boolean
		// thinking switch.
		if th, ok := out.Lookup("thinking"); !ok || th == nil {
			if isThinkingOff(effort) {
				out.Delete("reasoning_effort")
				sw := jsonx.NewObj()
				sw.Set("type", "disabled")
				out.Set("thinking", sw)
			}
		}
	case "low_high":
		disabled := false
		if th, ok := out.Obj("thinking"); ok {
			if t, _ := th.Str("type"); strings.EqualFold(t, "disabled") {
				disabled = true
			}
		}
		if disabled {
			out.Delete("thinking")
			out.Set("reasoning_effort", "low")
			break
		}
		if effort != "" {
			if isThinkingOff(effort) {
				out.Set("reasoning_effort", "low")
			} else if rank, ok := effortRank(effort); ok {
				if rank <= 1 {
					out.Set("reasoning_effort", "low")
				} else {
					out.Set("reasoning_effort", "high")
				}
			}
		}
	}
	return out
}

// PrepareBody renders the chat request body for one attempt.
//
// stream is forced to true: the gateway always consumes the upstream as an SSE
// stream and aggregates for clients that asked for a single response. Sending
// stream:false upstream makes the endpoint answer one JSON document, which the
// gateway's stream reader would read as "no chunks" (empty content, usage only).
func PrepareBody(payload *jsonx.Obj, model string) ([]byte, error) {
	return PrepareBodyWithHistoryLimit(payload, model, 0)
}

// PrepareBodyWithHistoryLimit trims old turns toward a byte limit on the outbound JSON.
func PrepareBodyWithHistoryLimit(payload *jsonx.Obj, model string, limit int) ([]byte, error) {
	body, _, err := PrepareBodyWithHistoryAccounting(payload, model, limit)
	return body, err
}

// PrepareBodyWithHistoryAccounting also estimates the input tokens removed by the history limit.
func PrepareBodyWithHistoryAccounting(payload *jsonx.Obj, model string, limit int) ([]byte, int, error) {
	out := NormalizeBody(payload, model)
	if out == nil {
		out = jsonx.NewObj()
	}
	out.Set("stream", true)
	if model != "" {
		// Always write the resolved wire id: the client may have named an alias
		// ("deepseek-v4.1-flash") or a path spelling, and letting the old value
		// through would make the upstream serve its own default model instead.
		out.Set("model", model)
	}
	body, err := jsonx.Marshal(out)
	if err != nil {
		return nil, 0, err
	}
	return chathistory.Apply(out, body, limit, nil)
}

// ChatHeaders builds the outbound headers of one chat attempt. The desktop
// client triple and the per-request id are part of the contract: without them
// the upstream sees a bare API client.
func ChatHeaders(cred Credential, requestID string) identity.Headers {
	h := identity.Headers{}
	h.Set("Content-Type", "application/json")
	// The LLM gateway answers with either response shape; the official client
	// sends */* and so must we.
	h.Set("Accept", "*/*")
	h.Set("Authorization", "Bearer "+StripBearer(cred.Token))
	if org := IAMOrg(cred); org != "" {
		h.Set("X-Org-Code", org)
	}
	for _, p := range clientHeaders("desktop") {
		h.Set(p.Name, p.Value)
	}
	if requestID == "" {
		requestID = RequestID()
	}
	h.Set("X-Request-Id", requestID)
	return h
}

// OutboundReasoning reports the reasoning level the body actually asks for,
// for request diagnostics: an explicit level first, then the boolean switch.
func OutboundReasoning(body *jsonx.Obj) (string, bool) {
	if body == nil {
		return "", false
	}
	if effort, ok := body.Str("reasoning_effort"); ok && effort != "" {
		return effort, true
	}
	if th, ok := body.Obj("thinking"); ok {
		if t, _ := th.Str("type"); strings.EqualFold(t, "enabled") {
			return "high", true
		} else if strings.EqualFold(t, "disabled") {
			return "off", true
		}
	}
	return "", false
}

// MarshalBody is a helper for tests and callers that hold a raw map.
func MarshalBody(v any) ([]byte, error) {
	return json.Marshal(v)
}
