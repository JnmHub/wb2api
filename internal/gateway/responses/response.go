package responses

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"wb2api/internal/gateway/leak"
	"wb2api/internal/jsonx"
)

// Env injects the sources of nondeterminism.
type Env struct {
	// NewID returns prefix + a unique suffix (nil = 32 random hex digits).
	NewID func(prefix string) string
	// Now returns unix seconds (nil = the wall clock).
	Now func() int64
}

func (e Env) id(prefix string) string {
	if e.NewID != nil {
		return e.NewID(prefix)
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

func (e Env) now() int64 {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now().Unix()
}

// dsmlCall is a tool call recovered from printed DSML text.
type dsmlCall struct{ id, name, args string }

// parseDSML is the legacy parse_dsml_tool_calls: call ids come from env, in
// order, so id consumption matches the original.
func (e Env) parseDSML(text string) (calls []dsmlCall, clean string) {
	cs, clean, ok := leak.ParseDSML(text)
	if !ok {
		return nil, text
	}
	for _, c := range cs {
		calls = append(calls, dsmlCall{id: e.id("call_"), name: c.Name, args: c.Arguments})
	}
	return calls, clean
}

func numOf(v any) float64 {
	switch t := v.(type) {
	case json.Number:
		f, _ := t.Float64()
		return f
	case float64:
		return t
	case bool:
		if t {
			return 1
		}
	}
	return 0
}

func orZero(v any) any {
	if tr(v) {
		return v
	}
	return num(0)
}

// Usage converts a chat usage block into the Responses shape (nil when empty).
func Usage(u *jsonx.Obj) *jsonx.Obj {
	if u == nil || u.Len() == 0 {
		return nil
	}
	det, _ := asObj(u.Get("completion_tokens_details"))
	pdet, _ := asObj(u.Get("prompt_tokens_details"))
	if det == nil {
		det = jsonx.NewObj()
	}
	if pdet == nil {
		pdet = jsonx.NewObj()
	}
	cached := first(first(u.Get("prompt_cache_hit_tokens"), det.Get("cached_tokens")), pdet.Get("cached_tokens"))
	return obj(
		"input_tokens", orZero(u.Get("prompt_tokens")),
		"input_tokens_details", obj("cached_tokens", orZero(cached)),
		"output_tokens", orZero(u.Get("completion_tokens")),
		"output_tokens_details", obj("reasoning_tokens", orZero(det.Get("reasoning_tokens"))),
		"total_tokens", orZero(u.Get("total_tokens")),
	)
}

func messageItem(env Env, text string) *jsonx.Obj {
	content := []any{}
	if text != "" {
		content = []any{obj("type", "output_text", "text", text, "annotations", []any{})}
	}
	return obj("id", env.id("msg_"), "type", "message", "status", "completed", "role", "assistant", "content", content)
}

// ChatToResponse folds a Chat Completions object into a Responses object.
// Calls to tools the client declared as custom are re-inflated into
// custom_tool_call items so clients such as Codex recognise them.
func ChatToResponse(chat *jsonx.Obj, model string, info Info, env Env) *jsonx.Obj {
	var choice *jsonx.Obj
	if cs, ok := chat.Get("choices").([]any); ok && len(cs) > 0 {
		choice, _ = asObj(cs[0])
	}
	if choice == nil {
		choice = jsonx.NewObj()
	}
	msg, _ := asObj(choice.Get("message"))
	if msg == nil {
		msg = jsonx.NewObj()
	}
	text := jsonx.PyStrIfTruthy(msg.Get("content"))
	reasoning := jsonx.PyStrIfTruthy(msg.Get("reasoning_content"))

	output := []any{}
	if reasoning != "" {
		output = append(output, obj("id", env.id("rs_"), "type", "reasoning", "status", "completed",
			"summary", []any{obj("type", "summary_text", "text", reasoning)}))
	}
	calls, _ := msg.Get("tool_calls").([]any)
	for _, raw := range calls {
		tc, ok := asObj(raw)
		if !ok {
			continue
		}
		fn, _ := asObj(tc.Get("function"))
		if fn == nil {
			fn = jsonx.NewObj()
		}
		callID := tc.Get("id")
		if !tr(callID) {
			callID = env.id("call_")
		}
		name := jsonx.PyStrIfTruthy(fn.Get("name"))
		if name != "" && info.CustomNames[name] {
			output = append(output, obj("id", env.id("ctc_"), "type", "custom_tool_call", "status", "completed",
				"call_id", callID, "name", name, "input", unwrapCustomInput(first(fn.Get("arguments"), ""))))
		} else {
			output = append(output, obj("id", env.id("fc_"), "type", "function_call", "status", "completed",
				"call_id", callID, "name", name, "arguments", first(fn.Get("arguments"), "{}")))
		}
	}
	if !tr(msg.Get("tool_calls")) {
		if dc, clean := env.parseDSML(text); len(dc) > 0 {
			for _, c := range dc {
				output = append(output, obj("id", env.id("fc_"), "type", "function_call", "status", "completed",
					"call_id", c.id, "name", c.name, "arguments", c.args))
			}
			text = clean
		}
	}
	if text != "" || len(output) == 0 {
		output = append(output, messageItem(env, text))
	}
	finish := jsonx.PyStr(first(choice.Get("finish_reason"), "stop"))
	status := "completed"
	if finish == "length" {
		status = "incomplete"
	}
	out := obj("id", env.id("resp_"), "object", "response", "created_at", num(env.now()), "status", status,
		"model", model, "output", output, "output_text", text, "metadata", jsonx.NewObj())
	applyNamespaceToCalls(output, info.Namespaces)
	out.Set("parallel_tool_calls", info.ParallelToolCalls)
	out.Set("tool_choice", info.ToolChoice)
	tools := info.Tools
	if tools == nil {
		tools = []any{}
	}
	out.Set("tools", tools)
	if uo, ok := asObj(chat.Get("usage")); ok {
		if u := Usage(uo); u != nil {
			out.Set("usage", u)
		}
	}
	if finish == "length" {
		out.Set("incomplete_details", obj("reason", "max_output_tokens"))
	}
	return out
}

// isTruncatedArguments: a non-empty argument string that is not valid JSON is
// a half-written call from a cut-off stream.
func isTruncatedArguments(raw string) bool {
	t := strings.TrimSpace(raw)
	return t != "" && !json.Valid([]byte(t))
}

// estimateTokens is the legacy CJK-aware estimate, used only when the
// upstream reports no usage.
func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	cjk, total := 0, 0
	for _, r := range text {
		total++
		if (r >= 0x4e00 && r <= 0x9fff) || (r >= 0x3400 && r <= 0x4dbf) {
			cjk++
		}
	}
	other := int(float64(total-cjk) / 3.6)
	if other < 1 {
		other = 1
	}
	return cjk + other
}

func runeSlice(s string, from, n int) string {
	// first n code points of s[from:] (from is a byte offset on a boundary)
	tail := s[from:]
	count := 0
	for i := range tail {
		if count == n {
			return tail[:i]
		}
		count++
	}
	return tail
}
