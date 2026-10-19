// Package shape turns a client's chat request into the body sent upstream. It
// is a port of legacy build_upstream_body() and the helpers it calls; the
// output is compared byte for byte with the Python implementation. Input
// objects are never modified: everything that changes is cloned first.
package shape

import (
	"encoding/json"
	"strconv"
	"strings"

	"wb2api/internal/jsonx"
)

// DefaultSystemPrompt is prepended when a conversation has no system message.
const DefaultSystemPrompt = "You are a helpful assistant."

// Catalog supplies per-model facts from the model catalogue. A nil Catalog
// behaves like a catalogue that knows nothing.
type Catalog interface {
	MaxOutputTokens(model string) int // 0 = unknown
	DefaultEffort(model string) string
}

// Options carries everything that is configuration rather than request data.
type Options struct {
	// PromptMode is passthrough (default), custom or append; PromptText is the
	// gateway prompt for custom/append (empty = leave messages alone).
	PromptMode string
	PromptText string
	// Degraded switches passthrough/append to the minimal neutral prompt.
	Degraded bool
	// SystemPrompt overrides DefaultSystemPrompt.
	SystemPrompt string
	// PreserveContract (WB_PRESERVE_REQUEST_CONTRACT) skips text rewriting of
	// tool results and tool-call arguments.
	PreserveContract bool
	Catalog          Catalog
	// Logf receives diagnostics (may be nil).
	Logf func(format string, args ...any)
}

func (o *Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

func (o *Options) systemPrompt() string {
	if o.SystemPrompt != "" {
		return o.SystemPrompt
	}
	return DefaultSystemPrompt
}

// ----------------------------------------------------------------- helpers

func asObj(v any) (*jsonx.Obj, bool) { o, ok := v.(*jsonx.Obj); return o, ok }

func isStr(v any) (string, bool) { s, ok := v.(string); return s, ok }

// pyInt mimics int(x) for the JSON scalars that can reach it.
func pyInt(v any) (int64, bool) {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i, true
		}
		if f, err := t.Float64(); err == nil {
			return int64(f), true
		}
	case string:
		if i, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
			return i, true
		}
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func num(i int64) json.Number { return json.Number(strconv.FormatInt(i, 10)) }

func lowerTrim(v any) string {
	s, _ := v.(string)
	return strings.ToLower(strings.TrimSpace(s))
}

// ------------------------------------------------------------------- build

// Build produces the upstream chat body. payload is the client's parsed
// request; the result is a new object.
func Build(payload *jsonx.Obj, opt Options) *jsonx.Obj {
	model := ""
	if m := payload.Get("model"); jsonx.Truthy(m) {
		model, _ = m.(string)
	}
	isDeepSeek := strings.HasPrefix(strings.ToLower(model), "deepseek")

	// Resolve the effective thinking state before the reasoning backfill.
	thinkingType := ""
	if th, ok := asObj(payload.Get("thinking")); ok {
		if t := th.Get("type"); jsonx.Truthy(t) {
			thinkingType = lowerTrim(t)
		}
	}
	effort := firstTruthy(payload.Get("reasoning_effort"), payload.Get("reasoningEffort"))
	thinkingEnabled := false
	if isDeepSeek {
		if thinkingType == "enabled" {
			thinkingEnabled = true
		} else if thinkingType != "disabled" && strings.ToLower(strings.TrimSpace(jsonx.PyStrIfTruthy(effort))) != "none" {
			thinkingEnabled = true
		}
	}

	msgs, _ := payload.Get("messages").([]any)
	if !jsonx.Truthy(payload.Get("messages")) {
		msgs = nil
	}
	msgs = normalizeRoles(msgs)
	msgs = applyPromptMode(msgs, &opt)
	msgs = sanitizeMessages(msgs, &opt)
	msgs = backfillReasoningContent(msgs, model, thinkingEnabled)
	if len(msgs) == 0 || roleOf(msgs[0]) != "system" {
		sys := jsonx.NewObj()
		sys.Set("role", "system")
		sys.Set("content", opt.systemPrompt())
		msgs = append([]any{sys}, msgs...)
	}

	body := payload.Clone()
	for _, k := range body.Keys() {
		if strings.HasPrefix(k, "_") {
			body.Delete(k)
		}
	}
	body.Set("model", model)

	msgs, _ = mergeAdjacentToolCalls(msgs)
	msgs, _ = repackToolResultBlocks(msgs)
	msgs, _ = cleanupOrphanToolCalls(msgs)
	body.Set("messages", msgs)

	translateMaxCompletionTokens(body)
	if body.Has("max_tokens") && strings.HasPrefix(strings.ToLower(model), "gpt-") {
		if mt, ok := pyInt(body.Get("max_tokens")); ok && mt < 16 {
			opt.logf("clamping %s max_tokens from %d to 16 for upstream requirements", model, mt)
			body.Set("max_tokens", num(16))
		}
	}
	normalizeToolChoice(body)
	normalizeTools(body)
	normalizeStop(body)
	if !body.Has("max_tokens") && opt.Catalog != nil {
		if n := opt.Catalog.MaxOutputTokens(model); n > 0 {
			body.Set("max_tokens", num(int64(n)))
		}
	}

	if isDeepSeek {
		optedOut := false
		if th, ok := asObj(body.Get("thinking")); ok {
			optedOut = lowerTrim(th.Get("type")) == "disabled"
		}
		eff := firstTruthy(body.Get("reasoning_effort"), body.Get("reasoningEffort"))
		if !optedOut && strings.ToLower(strings.TrimSpace(jsonx.PyStrIfTruthy(eff))) != "none" {
			if !body.Has("thinking") {
				th := jsonx.NewObj()
				th.Set("type", "enabled")
				body.Set("thinking", th)
			}
			if !jsonx.Truthy(eff) {
				def := ""
				if opt.Catalog != nil {
					def = opt.Catalog.DefaultEffort(model)
				}
				if def == "" {
					def = "high"
				}
				body.Set("reasoning_effort", def)
			}
		}
	}
	body.Set("stream", true)
	if !body.Has("stream_options") {
		so := jsonx.NewObj()
		so.Set("include_usage", true)
		body.Set("stream_options", so)
	}
	return body
}

// firstTruthy is Python's `a or b`.
func firstTruthy(a, b any) any {
	if jsonx.Truthy(a) {
		return a
	}
	return b
}

func roleOf(m any) string {
	if o, ok := asObj(m); ok {
		s, _ := o.Get("role").(string)
		return s
	}
	return ""
}

// ----------------------------------------------------------- message steps

func normalizeRoles(msgs []any) []any {
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		o, ok := asObj(m)
		if ok && o.Get("role") == "developer" {
			c := o.Clone()
			c.Set("role", "system")
			out = append(out, c)
			continue
		}
		out = append(out, m)
	}
	return out
}

func isSystemish(m any) bool {
	o, ok := asObj(m)
	if !ok {
		return false
	}
	r := o.Get("role")
	return r == "system" || r == "developer"
}

const degradedPrompt = "You are a helpful assistant. Respond in the user's language, " +
	"follow the user's instructions, and be direct and concise."

func sysMsg(text string) *jsonx.Obj {
	o := jsonx.NewObj()
	o.Set("role", "system")
	o.Set("content", text)
	return o
}

func rewritePrompt(msgs []any, text string) []any {
	if text == "" {
		return msgs
	}
	out := []any{sysMsg(text)}
	for _, m := range msgs {
		if !isSystemish(m) {
			out = append(out, m)
		}
	}
	return out
}

func appendPrompt(msgs []any, text string) []any {
	if text == "" {
		return msgs
	}
	at := 0
	for _, m := range msgs {
		if !isSystemish(m) {
			break
		}
		at++
	}
	out := make([]any, 0, len(msgs)+1)
	out = append(out, msgs[:at]...)
	out = append(out, sysMsg(text))
	return append(out, msgs[at:]...)
}

func applyPromptMode(msgs []any, opt *Options) []any {
	mode := strings.ToLower(strings.TrimSpace(opt.PromptMode))
	switch mode {
	case "custom", "append":
	default:
		mode = "passthrough"
	}
	if mode == "custom" || mode == "append" {
		if opt.PromptText == "" {
			return msgs
		}
		if mode == "custom" {
			return rewritePrompt(msgs, opt.PromptText)
		}
		if opt.Degraded {
			return rewritePrompt(msgs, degradedPrompt)
		}
		return appendPrompt(msgs, opt.PromptText)
	}
	if opt.Degraded {
		return rewritePrompt(msgs, degradedPrompt)
	}
	return msgs
}

func backfillReasoningContent(msgs []any, model string, thinkingEnabled bool) []any {
	if model == "" || !strings.HasPrefix(strings.ToLower(model), "deepseek") {
		return msgs
	}
	hasTrace := false
	for _, m := range msgs {
		o, ok := asObj(m)
		if !ok {
			continue
		}
		if r, ok := o.Get("reasoning").(string); ok && r != "" {
			hasTrace = true
			break
		}
		if o.Has("reasoning_content") {
			hasTrace = true
			break
		}
	}
	if !thinkingEnabled && !hasTrace {
		return msgs
	}
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		o, ok := asObj(m)
		if !ok || o.Get("role") != "assistant" {
			out = append(out, m)
			continue
		}
		item := o.Clone()
		rc, isStrRC := item.Get("reasoning_content").(string)
		if !isStrRC {
			legacy, _ := item.Get("reasoning").(string)
			rc = legacy
			item.Set("reasoning_content", rc)
		}
		if ex, ok := item.Get("reasoning").(string); !ok || ex == "" {
			if rc != "" {
				item.Set("reasoning", rc)
			} else {
				item.Set("reasoning", " ")
			}
		}
		out = append(out, item)
	}
	return out
}

func translateMaxCompletionTokens(obj *jsonx.Obj) {
	alias, had := obj.Lookup("max_completion_tokens")
	if !had {
		return
	}
	obj.Delete("max_completion_tokens")
	if alias == nil {
		return
	}
	if obj.Has("max_tokens") {
		return
	}
	if v, ok := pyInt(alias); ok && v > 0 {
		obj.Set("max_tokens", num(v))
	}
}

func normalizeStop(obj *jsonx.Obj) {
	val, ok := obj.Lookup("stop")
	if !ok {
		return
	}
	var out []any
	switch t := val.(type) {
	case string:
		if t != "" {
			out = []any{t}
		}
	case []any:
		for _, s := range t {
			if str, ok := s.(string); ok && str != "" {
				out = append(out, s)
			}
		}
	}
	if len(out) > 0 {
		obj.Set("stop", out)
	} else {
		obj.Delete("stop")
	}
}
