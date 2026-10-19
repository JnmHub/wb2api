package catalog

import (
	"encoding/json"
	"strconv"
	"strings"

	"wb2api/internal/jsonx"
)

// ------------------------------------------------------------------- limits

const defaultContext = 1_000_000

// contextFallback is the built-in knowledge table (panel measurements plus
// models.dev consensus), shared by both exits: model -> {context, output}.
var contextFallback = map[string][2]int64{
	"glm-5.2": {1000000, 131072}, "glm-5.1": {200000, 131072}, "glm-5.3": {1000000, 131072},
	"glm-5.3-flash": {1000000, 131072}, "glm-5v-turbo": {200000, 131072},
	"kimi-k2.7": {256000, 65536}, "kimi-k2.6": {256000, 262144}, "kimi-k2.5": {164000, 262144},
	"kimi-k3": {1048576, 131072}, "kimi-k2.8-preview": {1048576, 0},
	"minimax-m3": {512000, 512000}, "hy3": {192000, 64000}, "hy3-preview": {262144, 64000},
	"hy4-preview": {1000000, 64000}, "hy4-preview-x": {1000000, 64000},
	"deepseek-v4-pro": {1000000, 384000}, "deepseek-v4-flash": {1000000, 384000},
	"deepseek-v4.1-flash": {1000000, 384000},
	"gpt-6-astra":         {1050000, 128000}, "gpt-5.6-sol": {1050000, 128000},
	"gpt-5.6-terra": {1050000, 128000}, "gpt-5.6-luna": {1050000, 128000},
	"gpt-5.5": {1050000, 128000}, "gpt-5.4": {1050000, 128000},
	"gpt-5.3-codex": {400000, 128000}, "gemini-3.5-flash": {1048576, 65536},
	"auto": {168000, 0},
}

// BareID is how limit tables are keyed: lower-case, namespace stripped.
func BareID(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	return id
}

// pyInt is Python's int(x or 0) with failures mapped to 0.
func pyInt(v any) int64 {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		if f, err := t.Float64(); err == nil {
			return int64(f)
		}
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	case bool:
		if t {
			return 1
		}
	case string:
		if i, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
			return i
		}
	}
	return 0
}

// Limits resolves (context, output) for one model, per field: upstream value >
// knowledge table > local models.dev index > default (context 1M; output 0 =
// unknown and omitted). index may be nil.
func Limits(model string, remoteCtx, remoteOut int64, index map[string][2]int64) (ctx, out int64) {
	key := BareID(model)
	table := contextFallback[key]
	cached := index[key]
	ctx = firstPositive(remoteCtx, table[0], cached[0], defaultContext)
	out = firstPositive(remoteOut, table[1], cached[1], 0)
	return ctx, out
}

func firstPositive(vs ...int64) int64 {
	for _, v := range vs {
		if v > 0 {
			return v
		}
	}
	return 0
}

// ------------------------------------------------------------- /v1/models

// EntryEnv carries the non-metadata inputs of ModelEntry.
type EntryEnv struct {
	Created int64
	// Index is the local models.dev limit index (may be nil).
	Index map[string][2]int64
	// Clamp returns the measured upstream output clamp for a model (0 = unknown).
	Clamp func(model string) int64
}

func b(v bool) any { return v }

func strList(items ...string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

func obj(kv ...any) *jsonx.Obj {
	o := jsonx.NewObj()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

func num(n int64) json.Number { return json.Number(strconv.FormatInt(n, 10)) }

// ModelEntry builds the rich /v1/models entry of one model. The OpenAI spec
// only names id/object/created/owned_by, so capability data is emitted under
// every convention common clients look for.
func ModelEntry(id string, meta *jsonx.Obj, env EntryEnv) *jsonx.Obj {
	if meta == nil {
		meta = jsonx.NewObj()
	}
	item := obj("id", id, "object", "model", "created", num(env.Created), "owned_by", "workbuddy")
	if v := meta.Get("name"); jsonx.Truthy(v) {
		item.Set("name", v)
	}
	desc := meta.Get("descriptionEn")
	if !jsonx.Truthy(desc) {
		desc = meta.Get("descriptionZh")
	}
	if jsonx.Truthy(desc) {
		item.Set("description", desc)
	}

	vision := jsonx.Truthy(meta.Get("supportsImages")) && !jsonx.Truthy(meta.Get("disabledMultimodal"))
	if id == "glm-5.3-flash" {
		// upstream advertises multimodal but the backend has no vision channel
		vision = false
		item.Set("vision_note", "上游宣告多模态但实际通道不支持读图，已修正为纯文本")
	}
	tools := jsonx.Truthy(meta.Get("supportsToolCall"))
	thinks := jsonx.Truthy(meta.Get("supportsReasoning"))
	inputs := []string{"text"}
	if vision {
		inputs = append(inputs, "image")
	}

	item.Set("capabilities", obj("vision", b(vision), "tool_calls", b(tools), "reasoning", b(thinks)))
	item.Set("supports_vision", b(vision))
	item.Set("supports_images", b(vision))
	item.Set("supports_tool_calls", b(tools))
	item.Set("supports_reasoning", b(thinks))
	item.Set("vision", b(vision))
	item.Set("multimodal", b(vision))
	item.Set("abilities", obj("vision", b(vision), "functionCall", b(tools), "function_call", b(tools), "reasoning", b(thinks)))
	item.Set("input_modalities", strList(inputs...))
	item.Set("output_modalities", strList("text"))
	item.Set("modalities", obj("input", strList(inputs...), "output", strList("text")))
	item.Set("architecture", obj(
		"input_modalities", strList(inputs...),
		"output_modalities", strList("text"),
		"modality", strings.Join(inputs, "+")+"->text"))

	ctxV, outV := Limits(id, pyInt(meta.Get("maxInputTokens")), pyInt(meta.Get("maxOutputTokens")), env.Index)
	item.Set("context_length", num(ctxV))
	item.Set("max_input_tokens", num(ctxV))
	if outV > 0 {
		item.Set("max_output_tokens", num(outV))
		item.Set("max_completion_tokens", num(outV))
	}
	if env.Clamp != nil {
		if c := env.Clamp(id); c > 0 {
			item.Set("output_clamp", num(c))
			item.Set("max_output_tokens_clamped", num(c))
		}
	}
	if cw, ok := meta.Obj("contextWindow"); ok {
		if l := cw.Get("supportedLengths"); jsonx.Truthy(l) {
			item.Set("context_windows", l)
		}
	}

	if r, ok := meta.Obj("reasoning"); ok {
		if v := r.Get("supportedEfforts"); jsonx.Truthy(v) {
			item.Set("reasoning_efforts", v)
		}
		if v := r.Get("effort"); jsonx.Truthy(v) {
			item.Set("reasoning_fixed_effort", v)
		}
		if v := r.Get("defaultEffort"); jsonx.Truthy(v) {
			item.Set("reasoning_default_effort", v)
		}
		if v, has := r.Lookup("canDisableThinking"); has && v != nil {
			item.Set("reasoning_can_disable", v)
		}
	}
	if v, has := meta.Lookup("onlyReasoning"); has && v != nil {
		item.Set("always_reasoning", b(jsonx.Truthy(v)))
	}

	if v := meta.Get("credits"); jsonx.Truthy(v) {
		item.Set("credits", v)
	}
	if v := meta.Get("vendor"); jsonx.Truthy(v) {
		item.Set("vendor", v)
	}
	if v, has := meta.Lookup("temperature"); has && v != nil {
		item.Set("temperature", v)
	}
	if v, has := meta.Lookup("top_p"); has && v != nil {
		item.Set("top_p", v)
	}
	if jsonx.Truthy(meta.Get("isDefault")) {
		item.Set("is_default", true)
	}
	if tags, ok := meta.Arr("tags"); ok {
		var keep []any
		for _, t := range tags {
			if s, ok := t.(string); ok && !strings.HasPrefix(s, "badge:") {
				keep = append(keep, s)
			}
		}
		if len(keep) > 0 {
			item.Set("tags", keep)
		}
	}
	return item
}
