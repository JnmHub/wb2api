// Package responses translates between the OpenAI Responses API and Chat
// Completions. The upstream only speaks Chat Completions, so a /v1/responses
// request is translated down (ToChat) and the answer translated back up
// (ChatToResponse for a whole answer, Stream for server-sent events).
//
// It is a port of the Responses section of legacy wb_proxy.py and is pinned by
// golden vectors from that code (tools/genvectors/gen_responses.py). The package
// is pure: ids, the clock and the catalogue are injected so output can be
// compared byte for byte.
package responses

import (
	"encoding/json"
	"strconv"
	"strings"

	"wb2api/internal/jsonx"
)

// Options carries everything that is not request data.
type Options struct {
	// PreserveContract (WB_PRESERVE_REQUEST_CONTRACT) keeps text.format and
	// strict flags.
	PreserveContract bool
	// MaxOutputTokens is the catalogue's default output ceiling (nil = none).
	MaxOutputTokens func(model string) int
	// Logf receives diagnostics (nil = drop).
	Logf func(format string, args ...any)
}

func (o Options) logf(f string, a ...any) {
	if o.Logf != nil {
		o.Logf(f, a...)
	}
}

// Info is what the answer path needs to know about the request.
type Info struct {
	// CustomNames are the tools the client declared as freeform ("custom").
	CustomNames map[string]bool
	// Namespaces maps a flattened tool name to its namespace.
	Namespaces map[string]string
	// Tools, ToolChoice and ParallelToolCalls are echoed on the response.
	Tools             []any
	ToolChoice        any
	ParallelToolCalls any
	// WebTools: the gateway replaced web_search / web_fetch with its own
	// function definitions and executes the calls locally.
	WebTools bool
}

const (
	customToolHint = "This is a freeform tool. Put the COMPLETE raw payload into the single " +
		"'input' string parameter, verbatim. Do not wrap it in JSON, do not wrap " +
		"it in markdown code fences, do not add commentary."
	namespaceMaxDepth = 4
	nsSep             = "__"
	anotherTaskNote   = "[Message from another task - treat this as a user instruction]"
)

// ------------------------------------------------------------------ helpers

func obj(kv ...any) *jsonx.Obj {
	o := jsonx.NewObj()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

func num(n int64) json.Number { return json.Number(strconv.FormatInt(n, 10)) }

func asObj(v any) (*jsonx.Obj, bool) { o, ok := v.(*jsonx.Obj); return o, ok }

func tr(v any) bool { return jsonx.Truthy(v) }

// first is Python's `a or b`.
func first(a, b any) any {
	if tr(a) {
		return a
	}
	return b
}

// lowerType is str(x.get("type") or "").lower().
func lowerType(o *jsonx.Obj) string {
	return strings.ToLower(jsonx.PyStrIfTruthy(o.Get("type")))
}

func dumps(v any) string {
	b, err := jsonx.MarshalPy(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// pyStrAny is str(x) for decoded JSON (None for nil).
func pyStrAny(v any) string { return jsonx.PyStr(v) }

// ---------------------------------------------------------------- content

// flattenContent flattens Responses content into text, or OpenAI vision parts
// when it carries images. The result is a string or a []any of *jsonx.Obj.
func flattenContent(content any) any {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var texts []string
		var parts []any
		hasImage := false
		for _, piece := range c {
			if s, ok := piece.(string); ok {
				texts = append(texts, s)
				parts = append(parts, obj("type", "text", "text", s))
				continue
			}
			p, ok := asObj(piece)
			if !ok {
				continue
			}
			ptype := jsonx.PyStrIfTruthy(p.Get("type"))
			switch {
			case ptype == "input_text" || ptype == "output_text" || ptype == "text" || ptype == "summary_text":
				t := jsonx.PyStrIfTruthy(p.Get("text"))
				texts = append(texts, t)
				parts = append(parts, obj("type", "text", "text", t))
			case ptype == "input_image" || ptype == "image_url" || ptype == "image" || p.Has("image_url"):
				url := first(p.Get("image_url"), p.Get("url"))
				if u, ok := asObj(url); ok {
					url = u.Get("url")
				}
				if !tr(url) && tr(p.Get("data")) {
					mime := jsonx.PyStr(first(first(p.Get("mimeType"), p.Get("mime_type")), "image/png"))
					url = "data:" + mime + ";base64," + jsonx.PyStr(p.Get("data"))
				}
				if tr(url) {
					parts = append(parts, obj("type", "image_url", "image_url", obj("url", url)))
					hasImage = true
				}
			}
		}
		if hasImage {
			return parts
		}
		var nonEmpty []string
		for _, t := range texts {
			if t != "" {
				nonEmpty = append(nonEmpty, t)
			}
		}
		return strings.Join(nonEmpty, "\n")
	}
	return pyStrAny(content)
}

func isParts(v any) ([]any, bool) { l, ok := v.([]any); return l, ok }

// ------------------------------------------------------------------- tools

func isCustomTool(t any) bool {
	o, ok := asObj(t)
	return ok && lowerType(o) == "custom"
}

func subTools(entry *jsonx.Obj) []any {
	for _, k := range []string{"tools", "children", "functions"} {
		if l, ok := entry.Get(k).([]any); ok {
			return l
		}
	}
	return nil
}

// CustomToolNames lists the freeform tools of a request, descending into
// namespaces (newer Codex nests them in a "functions" namespace; missing one
// makes the answer come back as a function_call the client cannot dispatch).
func CustomToolNames(tools []any) map[string]bool {
	names := map[string]bool{}
	var walk func(tools []any, depth int)
	walk = func(tools []any, depth int) {
		if depth > namespaceMaxDepth {
			return
		}
		for _, t := range tools {
			o, ok := asObj(t)
			if !ok {
				continue
			}
			if isCustomTool(o) && tr(o.Get("name")) {
				names[jsonx.PyStr(o.Get("name"))] = true
			} else if lowerType(o) == "namespace" {
				walk(subTools(o), depth+1)
			}
		}
	}
	walk(tools, 0)
	return names
}

func downgradeCustomTool(t *jsonx.Obj) *jsonx.Obj {
	desc := jsonx.PyStrIfTruthy(t.Get("description"))
	extra := ""
	if f, ok := asObj(t.Get("format")); ok && tr(f.Get("definition")) {
		extra = "\n\nGrammar:\n" + jsonx.PyStr(f.Get("definition"))
	}
	return obj(
		"type", "function",
		"name", first(t.Get("name"), ""),
		"description", strings.TrimSpace(desc+"\n\n"+customToolHint+extra),
		"parameters", obj(
			"type", "object",
			"properties", obj("input", obj("type", "string", "description", "Complete raw payload for this tool, verbatim.")),
			"required", []any{"input"},
		),
	)
}

// expandNamespaceTools flattens namespace tools into plain function tools and
// remembers name -> namespace so answers can be stamped with it again.
func expandNamespaceTools(tools []any, opt Options) (flat []any, mapping map[string]string) {
	mapping = map[string]string{}
	seen := map[string]bool{}
	var collect func(entry any, depth int, ns string)
	collect = func(entry any, depth int, ns string) {
		e, ok := asObj(entry)
		if !ok || depth > namespaceMaxDepth {
			return
		}
		etype := lowerType(e)
		if etype == "namespace" {
			child := jsonx.PyStrIfTruthy(first(e.Get("name"), ns))
			for _, sub := range subTools(e) {
				collect(sub, depth+1, child)
			}
			return
		}
		if ns != "" && (etype == "" || etype == "function") {
			var fn *jsonx.Obj
			if f, ok := asObj(e.Get("function")); ok {
				fn = f
			} else {
				params := first(first(e.Get("parameters"), e.Get("input_schema")), nil)
				if !tr(params) {
					params = obj("type", "object", "properties", jsonx.NewObj())
				}
				fn = obj("name", e.Get("name"), "description", first(e.Get("description"), ""), "parameters", params)
				if opt.PreserveContract && e.Has("strict") {
					fn.Set("strict", e.Get("strict"))
				}
			}
			name := strings.TrimSpace(jsonx.PyStrIfTruthy(fn.Get("name")))
			if name == "" || seen[name] {
				return
			}
			seen[name] = true
			mapping[name] = ns
			params := fn.Get("parameters")
			if !tr(params) {
				params = obj("type", "object", "properties", jsonx.NewObj())
			}
			flatFn := obj("type", "function", "name", name, "description", first(fn.Get("description"), ""), "parameters", params)
			if fn.Has("strict") {
				flatFn.Set("strict", fn.Get("strict"))
			}
			flat = append(flat, flatFn)
			return
		}
		fn, _ := asObj(e.Get("function"))
		if fn == nil {
			fn = jsonx.NewObj()
		}
		name := strings.TrimSpace(jsonx.PyStrIfTruthy(first(e.Get("name"), fn.Get("name"))))
		if name != "" {
			if seen[name] {
				return
			}
			seen[name] = true
			if ns != "" {
				mapping[name] = ns
			}
		}
		flat = append(flat, e)
	}
	for _, t := range tools {
		collect(t, 0, "")
	}
	return flat, mapping
}

// resolveNamespacedName maps a name the model returned (js, ns__js, ns::js)
// back to (bare name, namespace).
func resolveNamespacedName(name string, mapping map[string]string) (string, string) {
	if name == "" {
		return name, ""
	}
	if ns, ok := mapping[name]; ok {
		return name, ns
	}
	if i := strings.Index(name, "::"); i > 0 {
		tail, head := name[i+2:], name[:i]
		if ns, ok := mapping[tail]; ok {
			return tail, ns
		}
		return tail, head
	}
	// exact comparison, so a namespace containing "__" is not split wrongly
	for tool, ns := range mapping {
		if name == ns+nsSep+tool {
			return tool, ns
		}
	}
	return name, ""
}

func stampNamespace(item *jsonx.Obj, mapping map[string]string) {
	if len(mapping) == 0 || item == nil {
		return
	}
	name, _ := item.Get("name").(string)
	if bare, ns := resolveNamespacedName(name, mapping); ns != "" {
		item.Set("name", bare)
		item.Set("namespace", ns)
	}
}

func applyNamespaceToCalls(items []any, mapping map[string]string) {
	if len(mapping) == 0 {
		return
	}
	for _, it := range items {
		o, ok := asObj(it)
		if !ok {
			continue
		}
		if t, _ := o.Get("type").(string); t != "function_call" && t != "custom_tool_call" {
			continue
		}
		if tr(o.Get("namespace")) {
			continue
		}
		name, _ := o.Get("name").(string)
		if bare, ns := resolveNamespacedName(name, mapping); ns != "" {
			o.Set("name", bare)
			o.Set("namespace", ns)
		}
	}
}

// additionalToolsFromInput collects the tools newer Codex Desktop puts into
// {"type":"additional_tools"} input items instead of the top level.
func additionalToolsFromInput(payload *jsonx.Obj) []any {
	var out []any
	input, _ := payload.Get("input").([]any)
	for _, it := range input {
		if o, ok := asObj(it); ok {
			if t, _ := o.Get("type").(string); t == "additional_tools" {
				if sub, ok := o.Get("tools").([]any); ok {
					out = append(out, sub...)
				}
			}
		}
	}
	return out
}

func toolsForChat(tools []any) []any {
	out := []any{}
	for _, t := range tools {
		o, ok := asObj(t)
		if !ok {
			continue
		}
		if isCustomTool(o) {
			out = append(out, downgradeCustomTool(o))
		} else {
			out = append(out, o)
		}
	}
	return out
}

// ----------------------------------------------------------------- messages

func lastRole(messages []*jsonx.Obj) string {
	if len(messages) == 0 {
		return ""
	}
	r, _ := messages[len(messages)-1].Get("role").(string)
	return r
}

// toolOutputContent is the shared conversion of a function/custom tool output.
func toolOutputContent(raw any, allowImageDict bool) any {
	switch o := raw.(type) {
	case []any:
		return flattenContent(o)
	case *jsonx.Obj:
		if allowImageDict {
			t := jsonx.PyStrIfTruthy(o.Get("type"))
			if t == "input_image" || t == "image_url" || t == "image" || o.Has("image_url") {
				return flattenContent([]any{o})
			}
		}
		return dumps(o)
	case string:
		return o
	}
	return pyStrAny(raw)
}

func inputToMessages(payload *jsonx.Obj, opt Options) []*jsonx.Obj {
	var messages []*jsonx.Obj
	if s, ok := payload.Get("instructions").(string); ok && strings.TrimSpace(s) != "" {
		messages = append(messages, obj("role", "system", "content", s))
	}
	pendingReasoning := ""
	addToolCall := func(tc *jsonx.Obj) {
		if lastRole(messages) == "assistant" {
			prev := messages[len(messages)-1]
			if calls, ok := prev.Get("tool_calls").([]any); ok {
				prev.Set("tool_calls", append(calls, tc))
			} else {
				prev.Set("tool_calls", []any{tc})
			}
			if pendingReasoning != "" && !prev.Has("reasoning_content") {
				prev.Set("reasoning_content", pendingReasoning)
				pendingReasoning = ""
			}
			return
		}
		m := obj("role", "assistant", "content", "", "tool_calls", []any{tc})
		if pendingReasoning != "" {
			m.Set("reasoning_content", pendingReasoning)
			pendingReasoning = ""
		}
		messages = append(messages, m)
	}

	switch inp := payload.Get("input").(type) {
	case string:
		messages = append(messages, obj("role", "user", "content", inp))
	case []any:
		for _, rawItem := range inp {
			if s, ok := rawItem.(string); ok {
				messages = append(messages, obj("role", "user", "content", s))
				continue
			}
			item, ok := asObj(rawItem)
			if !ok {
				continue
			}
			itype, typed := item.Get("type").(string)
			if !typed && item.Get("type") != nil {
				itype, typed = "\x00non-string", true
			}
			switch {
			case !typed || itype == "message" || itype == "user":
				body := flattenContent(item.Get("content"))
				if !tr(body) {
					continue
				}
				role := jsonx.PyStr(first(item.Get("role"), "user"))
				if role == "developer" {
					role = "system"
				}
				if role == "assistant" && lastRole(messages) == "assistant" {
					prev := messages[len(messages)-1]
					if tr(prev.Get("content")) {
						prev.Set("content", pyStrAny(prev.Get("content"))+"\n"+pyStrAny(body))
					} else {
						prev.Set("content", body)
					}
					if pendingReasoning != "" && !prev.Has("reasoning_content") {
						prev.Set("reasoning_content", pendingReasoning)
						pendingReasoning = ""
					}
				} else {
					m := obj("role", role, "content", body)
					if role == "assistant" && pendingReasoning != "" {
						m.Set("reasoning_content", pendingReasoning)
						pendingReasoning = ""
					}
					messages = append(messages, m)
				}

			case itype == "reasoning":
				text := ""
				switch summ := item.Get("summary").(type) {
				case []any:
					var ts []string
					for _, p := range summ {
						if po, ok := asObj(p); ok && tr(po.Get("text")) {
							ts = append(ts, jsonx.PyStr(po.Get("text")))
						}
					}
					text = strings.Join(ts, "\n")
				case string:
					text = summ
				}
				if text == "" {
					switch c := item.Get("content").(type) {
					case string:
						text = c
					case []any:
						if s, ok := flattenContent(c).(string); ok {
							text = s
						}
					}
				}
				if text != "" {
					if lastRole(messages) == "assistant" {
						messages[len(messages)-1].Set("reasoning_content", text)
					} else {
						pendingReasoning = text
					}
				}

			case itype == "function_call_output":
				raw := item.Get("output")
				if !tr(item.Get("call_id")) {
					var txt any
					switch o := raw.(type) {
					case []any:
						txt = flattenContent(o)
					case *jsonx.Obj:
						txt = dumps(o)
					default:
						txt = jsonx.PyStrIfTruthy(raw)
					}
					if parts, isList := isParts(txt); isList {
						opt.logf("[wb-proxy] orphan function_call_output kept as multimodal parts (%d)", len(parts))
						content := append([]any{obj("type", "text", "text", anotherTaskNote)}, parts...)
						messages = append(messages, obj("role", "user", "content", content))
						continue
					}
					if s := strings.TrimSpace(txt.(string)); s != "" {
						messages = append(messages, obj("role", "user", "content",
							"[Message from another task - treat this as a user instruction]\n\n"+s))
						continue
					}
				}
				messages = append(messages, obj("role", "tool", "tool_call_id", first(item.Get("call_id"), ""),
					"content", toolOutputContent(raw, true)))

			case itype == "function_call":
				addToolCall(obj("id", first(item.Get("call_id"), first(item.Get("id"), "")), "type", "function",
					"function", obj("name", first(item.Get("name"), ""), "arguments", first(item.Get("arguments"), "{}"))))

			case itype == "custom_tool_call":
				in := item.Get("input")
				var raw string
				switch v := in.(type) {
				case *jsonx.Obj, []any:
					raw = dumps(v)
				case string:
					raw = v
				case nil:
					raw = ""
				default:
					raw = pyStrAny(v)
				}
				addToolCall(obj("id", first(item.Get("call_id"), first(item.Get("id"), "")), "type", "function",
					"function", obj("name", first(item.Get("name"), ""), "arguments", dumps(obj("input", raw)))))

			case itype == "custom_tool_call_output":
				messages = append(messages, obj("role", "tool", "tool_call_id", first(item.Get("call_id"), ""),
					"content", toolOutputContent(item.Get("output"), false)))

			case itype == "agent_message":
				text := ""
				if parts, ok := item.Get("content").([]any); ok {
					var ss []string
					for _, p := range parts {
						if po, ok := asObj(p); ok {
							ss = append(ss, jsonx.PyStrIfTruthy(first(po.Get("text"), po.Get("encrypted_content"))))
						} else {
							ss = append(ss, pyStrAny(p))
						}
					}
					text = strings.TrimSpace(strings.Join(ss, "\n"))
				} else {
					text = strings.TrimSpace(jsonx.PyStrIfTruthy(item.Get("content")))
				}
				if text != "" {
					messages = append(messages, obj("role", "user", "content",
						"[Message from another task - treat this as a user instruction]\n\n"+text))
				}

			case itype == "additional_tools":
				// merged into chat["tools"] by ToChat

			default:
				keys := item.Keys()
				if len(keys) > 8 {
					keys = keys[:8]
				}
				opt.logf("responses: WARNING unhandled input item type=%v keys=%v", item.Get("type"), keys)
			}
		}
	}
	return messages
}

// ToChat translates a Responses request into a Chat Completions body.
func ToChat(payload *jsonx.Obj, opt Options) (*jsonx.Obj, Info) {
	msgs := inputToMessages(payload, opt)
	list := make([]any, len(msgs))
	for i, m := range msgs {
		list[i] = m
	}
	chat := obj("model", payload.Get("model"), "messages", list)

	if opt.PreserveContract {
		if text, ok := asObj(payload.Get("text")); ok {
			if f, ok := asObj(text.Get("format")); ok {
				switch kind, _ := f.Get("type").(string); kind {
				case "text", "json_object":
					chat.Set("response_format", obj("type", kind))
				case "json_schema":
					js := jsonx.NewObj()
					for _, k := range []string{"name", "description", "schema", "strict"} {
						if f.Has(k) {
							js.Set(k, f.Get(k))
						}
					}
					chat.Set("response_format", obj("type", kind, "json_schema", js))
				}
			}
		}
	}
	for _, k := range []string{"temperature", "top_p", "seed"} {
		if v, ok := payload.Lookup(k); ok && v != nil {
			chat.Set(k, v)
		}
	}
	if v, ok := payload.Lookup("max_output_tokens"); ok && v != nil {
		chat.Set("max_tokens", v)
	} else if opt.MaxOutputTokens != nil {
		if model, _ := payload.Get("model").(string); model != "" {
			if n := opt.MaxOutputTokens(model); n > 0 {
				chat.Set("max_tokens", num(int64(n)))
			}
		}
	}
	var effort any
	if r, ok := asObj(payload.Get("reasoning")); ok {
		effort = r.Get("effort")
	}
	if !tr(effort) {
		effort = payload.Get("reasoning_effort")
	}
	if tr(effort) {
		chat.Set("reasoning_effort", effort)
	}

	var raw []any
	if top, ok := payload.Get("tools").([]any); ok {
		raw = append(raw, top...)
	}
	raw = append(raw, additionalToolsFromInput(payload)...)
	info := Info{CustomNames: CustomToolNames(raw), Tools: raw,
		ToolChoice: "auto", ParallelToolCalls: true}
	if payload.Has("tool_choice") {
		info.ToolChoice = payload.Get("tool_choice")
	}
	if payload.Has("parallel_tool_calls") {
		info.ParallelToolCalls = payload.Get("parallel_tool_calls")
	}
	if len(raw) > 0 {
		flat, mapping := expandNamespaceTools(raw, opt)
		chat.Set("tools", toolsForChat(flat))
		info.Namespaces = mapping
	}
	if tr(payload.Get("tool_choice")) {
		chat.Set("tool_choice", payload.Get("tool_choice"))
	}
	if v, ok := payload.Lookup("parallel_tool_calls"); ok && v != nil {
		chat.Set("parallel_tool_calls", v)
	}
	return chat, info
}

// unwrapCustomInput pulls the freeform string back out of an {"input": ...}
// argument blob.
func unwrapCustomInput(args any) string {
	s, ok := args.(string)
	if !ok {
		if !tr(args) {
			return dumps("")
		}
		return dumps(args)
	}
	parsed, err := jsonx.Unmarshal([]byte(s))
	if err != nil {
		return s
	}
	switch p := parsed.(type) {
	case *jsonx.Obj:
		v := p.Get("input")
		if str, ok := v.(string); ok {
			return str
		}
		if v != nil {
			return dumps(v)
		}
	case string:
		return p
	}
	return s
}
