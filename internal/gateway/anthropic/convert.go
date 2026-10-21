// Package anthropic translates between the Anthropic Messages API and Chat
// Completions (port of the Messages section of legacy wb_proxy.py, pinned by
// golden vectors from tools/genvectors/gen_anthropic.py). Like package
// responses it is pure: ids and the catalogue are injected.
package anthropic

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"wb2api/internal/jsonx"
	"wb2api/internal/usage"
)

// Env injects the sources of nondeterminism.
type Env struct {
	NewID func(prefix string) string
}

func (e Env) id(prefix string) string {
	if e.NewID != nil {
		return e.NewID(prefix)
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

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

// text is _anthropic_text: scalars become their Python str(), everything else "".
func text(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool, json.Number:
		return jsonx.PyStr(t)
	}
	return ""
}

// jsonText is _anthropic_json_text: the compact JSON of a value ("{}" for none).
func jsonText(v any) string {
	switch t := v.(type) {
	case nil:
		return "{}"
	case string:
		return t
	}
	b, err := jsonx.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// parseToolInput is _anthropic_parse_tool_input.
func parseToolInput(v any) any {
	switch t := v.(type) {
	case nil:
		return jsonx.NewObj()
	case *jsonx.Obj:
		return t
	case string:
		if t == "" {
			return jsonx.NewObj()
		}
		parsed, err := jsonx.Unmarshal([]byte(t))
		if err != nil {
			return obj("raw", t)
		}
		if o, ok := parsed.(*jsonx.Obj); ok {
			return o
		}
		return obj("value", parsed)
	}
	return obj("value", v)
}

func blockText(v any) string {
	b, ok := asObj(v)
	if !ok {
		return ""
	}
	if b.Has("text") {
		return text(b.Get("text"))
	}
	if b.Has("content") {
		return contentToText(b.Get("content"), false)
	}
	return ""
}

func imagePart(block *jsonx.Obj) *jsonx.Obj {
	url := ""
	if src, ok := asObj(block.Get("source")); ok {
		switch jsonx.PyStrIfTruthy(src.Get("type")) {
		case "base64":
			media := text(first(src.Get("media_type"), "image/png"))
			url = "data:" + media + ";base64," + text(src.Get("data"))
		case "url":
			url = text(src.Get("url"))
		case "file":
			url = text(first(src.Get("file_id"), src.Get("url")))
		}
	}
	return obj("type", "image_url", "image_url", obj("url", url))
}

func first(a, b any) any {
	if tr(a) {
		return a
	}
	return b
}

func imageURL(part *jsonx.Obj) string {
	if iu, ok := asObj(part.Get("image_url")); ok {
		return text(iu.Get("url"))
	}
	return ""
}

func documentText(v any) string {
	b, ok := asObj(v)
	if !ok {
		return ""
	}
	title := text(b.Get("title"))
	src, _ := asObj(b.Get("source"))
	if src == nil {
		src = jsonx.NewObj()
	}
	body := ""
	switch jsonx.PyStrIfTruthy(src.Get("type")) {
	case "text":
		body = text(src.Get("data"))
	case "content":
		body = contentToText(src.Get("content"), false)
	case "base64":
		body = "[document " + text(first(src.Get("media_type"), "application/octet-stream")) + " omitted]"
	case "url":
		body = text(src.Get("url"))
	case "file":
		body = text(first(src.Get("file_id"), src.Get("url")))
	}
	if title != "" {
		return title + "\n" + body
	}
	return body
}

func contentToText(value any, isError bool) string {
	var out string
	switch t := value.(type) {
	case string:
		out = t
	case []any:
		var parts []string
		for _, it := range t {
			item, ok := asObj(it)
			if !ok {
				continue
			}
			switch jsonx.PyStrIfTruthy(item.Get("type")) {
			case "image":
				parts = append(parts, imageURL(imagePart(item)))
			case "document":
				parts = append(parts, documentText(item))
			default:
				parts = append(parts, blockText(item))
			}
		}
		out = strings.Join(parts, "")
	case *jsonx.Obj:
		out = blockText(t)
	default:
		out = text(value)
	}
	if isError {
		return "error: " + out
	}
	return out
}

// toolResultContent keeps an image in a tool result as a structured part
// (upstream bills a data URL as text at roughly a token per character); pure
// text collapses to one string. isErr is the literal `is_error is True`.
func toolResultContent(value any, isErr bool) any {
	list, ok := value.([]any)
	if !ok {
		return contentToText(value, isErr)
	}
	var parts []any
	hasImage := false
	for _, it := range list {
		item, ok := asObj(it)
		if !ok {
			if s, isStr := it.(string); isStr {
				parts = append(parts, obj("type", "text", "text", s))
			}
			continue
		}
		switch jsonx.PyStrIfTruthy(item.Get("type")) {
		case "image":
			part := imagePart(item)
			if imageURL(part) != "" {
				hasImage = true
				parts = append(parts, part)
			} else {
				parts = append(parts, obj("type", "text", "text", "[image omitted: no data]"))
			}
		case "document":
			parts = append(parts, obj("type", "text", "text", documentText(item)))
		default:
			if t := blockText(item); t != "" {
				parts = append(parts, obj("type", "text", "text", t))
			}
		}
	}
	if !hasImage {
		var sb strings.Builder
		for _, p := range parts {
			if po, _ := asObj(p); po != nil && po.Get("type") == "text" {
				sb.WriteString(text(po.Get("text")))
			}
		}
		if isErr {
			return "error: " + sb.String()
		}
		return sb.String()
	}
	if isErr {
		parts = append([]any{obj("type", "text", "text", "error:")}, parts...)
	}
	return parts
}

func systemText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var parts []string
		for _, it := range t {
			if o, ok := asObj(it); ok {
				if s := text(o.Get("text")); s != "" {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func toolChoice(v any) (any, bool) {
	switch t := v.(type) {
	case string:
		val := strings.ToLower(strings.TrimSpace(t))
		switch val {
		case "auto":
			return "auto", true
		case "any", "required":
			return "required", true
		case "none":
			return "none", true
		}
		return val, true
	case *jsonx.Obj:
		switch strings.ToLower(strings.TrimSpace(jsonx.PyStrIfTruthy(t.Get("type")))) {
		case "auto":
			return "auto", true
		case "any":
			return "required", true
		case "none":
			return "none", true
		case "tool":
			return obj("type", "function", "function", obj("name", text(t.Get("name")))), true
		}
	}
	return nil, false
}

func toolsToChat(tools any) (out []any, skipped []string) {
	list, ok := tools.([]any)
	if !ok {
		return nil, nil
	}
	for _, it := range list {
		item, ok := asObj(it)
		if !ok {
			continue
		}
		if _, hasFn := asObj(item.Get("function")); hasFn {
			out = append(out, item)
			continue
		}
		name := text(first(item.Get("name"), item.Get("type")))
		schema, has := item.Lookup("input_schema")
		if !has || schema == nil {
			schema, has = item.Lookup("parameters")
		}
		if !has || schema == nil {
			if name != "" {
				skipped = append(skipped, name)
			}
			continue
		}
		fn := obj("name", name)
		if d, ok := item.Lookup("description"); ok && d != nil {
			fn.Set("description", d)
		}
		fn.Set("parameters", schema)
		out = append(out, obj("type", "function", "function", fn))
	}
	return out, skipped
}

func effortFromMessages(payload *jsonx.Obj) string {
	if oc, ok := asObj(payload.Get("output_config")); ok {
		if e := strings.TrimSpace(text(oc.Get("effort"))); e != "" {
			return e
		}
	}
	th, ok := asObj(payload.Get("thinking"))
	if !ok {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(text(th.Get("type")))) {
	case "disabled":
		return "none"
	case "enabled":
	default:
		return ""
	}
	budget := toInt(th.Get("budget_tokens"))
	switch {
	case budget <= 0:
		return ""
	case budget >= 32000:
		return "xhigh"
	case budget >= 16000:
		return "high"
	case budget >= 8000:
		return "medium"
	}
	return "low"
}

func toInt(v any) int64 {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		if f, err := t.Float64(); err == nil {
			return int64(f)
		}
	case string:
		if i, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
			return i
		}
	case bool:
		if t {
			return 1
		}
	}
	return 0
}

func blocksToMessages(role string, blocks []any) []*jsonx.Obj {
	var toolCalls, contentParts []any
	var toolMsgs []*jsonx.Obj
	var texts []string
	hasNonText := false
	for _, b := range blocks {
		block, ok := asObj(b)
		if !ok {
			continue
		}
		typ := jsonx.PyStrIfTruthy(block.Get("type"))
		switch typ {
		case "thinking", "redacted_thinking":
			continue
		case "tool_use", "server_tool_use":
			toolCalls = append(toolCalls, obj("id", text(block.Get("id")), "type", "function",
				"function", obj("name", text(block.Get("name")), "arguments", jsonText(block.Get("input")))))
			continue
		case "tool_result":
			isErr := false
			if v, ok := block.Get("is_error").(bool); ok && v {
				isErr = true
			}
			toolMsgs = append(toolMsgs, obj("role", "tool", "tool_call_id", text(block.Get("tool_use_id")),
				"content", toolResultContent(block.Get("content"), isErr)))
			continue
		case "image":
			part := imagePart(block)
			if imageURL(part) == "" {
				continue
			}
			hasNonText = true
			contentParts = append(contentParts, part)
			continue
		case "document":
			hasNonText = true
			contentParts = append(contentParts, obj("type", "text", "text", documentText(block)))
			continue
		}
		t := blockText(block)
		texts = append(texts, t)
		contentParts = append(contentParts, obj("type", "text", "text", t))
	}
	out := toolMsgs
	if len(toolCalls) == 0 && len(texts) == 0 && !hasNonText {
		return out
	}
	if role == "" {
		role = "user"
	}
	msg := obj("role", role)
	if len(toolCalls) > 0 {
		msg.Set("tool_calls", toolCalls)
	}
	switch {
	case hasNonText:
		msg.Set("content", contentParts)
	case len(texts) > 0:
		msg.Set("content", strings.Join(texts, ""))
	default:
		msg.Set("content", "")
	}
	return append(out, msg)
}

// Options carries the catalogue.
type Options struct {
	MaxOutputTokens func(model string) int
}

// ErrInvalid is a client mistake reported as an invalid_request_error.
type ErrInvalid struct{ Msg string }

func (e *ErrInvalid) Error() string { return e.Msg }

func invalid(f string, a ...any) error { return &ErrInvalid{fmt.Sprintf(f, a...)} }

// ToChat translates an Anthropic Messages request into Chat Completions.
func ToChat(payload *jsonx.Obj, opt Options) (*jsonx.Obj, error) {
	model := strings.TrimSpace(text(payload.Get("model")))
	if model == "" {
		return nil, invalid("model is required")
	}
	in, ok := payload.Get("messages").([]any)
	if !ok {
		return nil, invalid("messages must be a list")
	}
	if len(in) == 0 {
		return nil, invalid("messages must not be empty")
	}
	chat := obj("model", model, "messages", []any{})
	for _, k := range []string{"temperature", "top_p"} {
		if v, ok := payload.Lookup(k); ok && v != nil {
			chat.Set(k, v)
		}
	}
	if v, ok := payload.Lookup("max_tokens"); ok && v != nil {
		chat.Set("max_tokens", v)
	} else if opt.MaxOutputTokens != nil {
		if n := opt.MaxOutputTokens(model); n > 0 {
			chat.Set("max_tokens", num(int64(n)))
		}
	}
	if v, ok := payload.Lookup("stream"); ok && v != nil {
		chat.Set("stream", jsonx.Truthy(v))
	}
	if stops, ok := payload.Get("stop_sequences").([]any); ok && len(stops) > 0 {
		chat.Set("stop", stops)
	}
	if md, ok := asObj(payload.Get("metadata")); ok {
		if v, has := md.Lookup("user_id"); has && v != nil {
			chat.Set("user", v)
		}
	}
	tools, skipped := toolsToChat(payload.Get("tools"))
	if len(tools) > 0 {
		chat.Set("tools", tools)
	}
	if c, ok := toolChoice(payload.Get("tool_choice")); ok {
		chat.Set("tool_choice", c)
	}
	if rc, ok := asObj(payload.Get("tool_choice")); ok && rc.Get("disable_parallel_tool_use") == true {
		chat.Set("parallel_tool_calls", false)
	}
	if e := effortFromMessages(payload); e != "" {
		chat.Set("reasoning_effort", e)
	}
	var messages []any
	system := systemText(payload.Get("system"))
	if len(skipped) > 0 {
		note := "These Anthropic server tools are not available here: " + strings.Join(skipped, ", ") + ". Do not call them."
		if system != "" {
			system += "\n" + note
		} else {
			system = note
		}
	}
	if system != "" {
		messages = append(messages, obj("role", "system", "content", system))
	}
	for idx, it := range in {
		item, ok := asObj(it)
		if !ok {
			continue
		}
		role := strings.TrimSpace(text(item.Get("role")))
		if role == "" {
			role = "user"
		}
		content := item.Get("content")
		if role == "system" || role == "developer" {
			// newer Claude Code carries system messages inside `messages`
			t := systemText(content)
			if t == "" {
				t = contentToText(content, false)
			}
			if t != "" {
				messages = append(messages, obj("role", "system", "content", t))
			}
			continue
		}
		if role != "user" && role != "assistant" {
			return nil, invalid("messages[%d].role %s must be user or assistant; pass a system prompt in the top-level system field", idx, pyRepr(role))
		}
		switch c := content.(type) {
		case string:
			messages = append(messages, obj("role", role, "content", c))
		case []any:
			for _, m := range blocksToMessages(role, c) {
				messages = append(messages, m)
			}
		case nil:
		default:
			if t := text(content); t != "" {
				messages = append(messages, obj("role", role, "content", t))
			}
		}
	}
	chat.Set("messages", messages)
	return chat, nil
}

// pyRepr renders a string like Python's repr (single quotes).
func pyRepr(s string) string {
	if !strings.Contains(s, "'") {
		return "'" + s + "'"
	}
	if !strings.Contains(s, `"`) {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `\'`) + "'"
}

// ------------------------------------------------------------------- errors

// ErrorType maps an HTTP status to the Anthropic error.type vocabulary.
func ErrorType(status int) string {
	switch status {
	case 400, 422:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 429:
		return "rate_limit_error"
	case 503, 529:
		return "overloaded_error"
	}
	return "api_error"
}

// ErrorObj is the Anthropic error envelope.
func ErrorObj(status int, message, errType string) *jsonx.Obj {
	if errType == "" {
		errType = ErrorType(status)
	}
	if message == "" {
		message = "upstream error"
	}
	return obj("type", "error", "error", obj("type", errType, "message", message))
}

// ---------------------------------------------------------- whole responses

func usageOf(u *jsonx.Obj, cached func(*jsonx.Obj) int64) *jsonx.Obj {
	out := obj("input_tokens", num(0), "cache_creation_input_tokens", num(0), "cache_read_input_tokens", num(0), "output_tokens", num(0))
	if u == nil {
		return out
	}
	var c int64
	if v, ok := u.Lookup("prompt_cache_hit_tokens"); ok && v != nil {
		c = toInt(v)
	} else if cached != nil {
		c = cached(u)
	}
	write := toInt(u.Get("prompt_cache_write_tokens"))
	// Anthropic reports non-cached input in input_tokens and cached tokens in
	// the two cache_* fields separately; WorkBuddy's prompt_tokens includes
	// them, so subtract to avoid double-counting the context.
	input := toInt(u.Get("prompt_tokens")) - c - write
	if input < 0 {
		input = 0
	}
	out.Set("input_tokens", num(input))
	out.Set("output_tokens", num(toInt(u.Get("completion_tokens"))))
	out.Set("cache_read_input_tokens", num(c))
	out.Set("cache_creation_input_tokens", num(write))
	if st := u.Get("service_tier"); tr(st) {
		out.Set("service_tier", st)
	}
	return out
}

func messageID(env Env, v any) string {
	s := text(v)
	if s == "" {
		return env.id("msg_")
	}
	if strings.HasPrefix(s, "msg_") {
		return s
	}
	return "msg_" + s
}

func stopReason(finish any) string {
	switch strings.ToLower(strings.TrimSpace(jsonx.PyStrIfTruthy(finish))) {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter", "refusal":
		return "refusal"
	}
	return "end_turn"
}

// Cached extracts the best cache-hit value of a usage block (set by the caller:
// it lives in package usage).
type Cached func(*jsonx.Obj) int64

// ChatToMessages folds a Chat Completions object into an Anthropic Messages
// object.
func ChatToMessages(chat *jsonx.Obj, env Env, cached Cached) *jsonx.Obj {
	if chat == nil {
		chat = jsonx.NewObj()
	}
	msg := jsonx.NewObj()
	var finish any = ""
	if cs, ok := chat.Get("choices").([]any); ok && len(cs) > 0 {
		if c0, ok := asObj(cs[0]); ok {
			if m, ok := asObj(c0.Get("message")); ok {
				msg = m
			}
			finish = first(c0.Get("finish_reason"), "")
		}
	}
	content := []any{}
	if r := text(msg.Get("reasoning_content")); r != "" {
		content = append(content, obj("type", "thinking", "thinking", r))
	}
	var t string
	switch c := msg.Get("content").(type) {
	case []any:
		var sb strings.Builder
		for _, p := range c {
			if po, ok := asObj(p); ok {
				sb.WriteString(blockText(po))
			}
		}
		t = sb.String()
	default:
		t = text(c)
	}
	if t != "" {
		content = append(content, obj("type", "text", "text", t))
	}
	if calls, ok := msg.Get("tool_calls").([]any); ok {
		for _, raw := range calls {
			call, ok := asObj(raw)
			if !ok {
				continue
			}
			fn, _ := asObj(call.Get("function"))
			if fn == nil {
				fn = jsonx.NewObj()
			}
			id := text(call.Get("id"))
			if id == "" {
				id = env.id("toolu_")
			}
			content = append(content, obj("type", "tool_use", "id", id, "name", text(fn.Get("name")),
				"input", parseToolInput(fn.Get("arguments"))))
		}
	}
	var u *jsonx.Obj
	if uo, ok := asObj(chat.Get("usage")); ok {
		u = uo
	}
	return obj("id", messageID(env, chat.Get("id")), "type", "message", "role", "assistant",
		"model", text(chat.Get("model")), "content", content,
		"stop_reason", stopReason(finish), "stop_sequence", nil, "usage", usageOf(u, cached))
}

// EstimateChatTokens is the count_tokens estimate (CJK-aware, intentionally not
// presented as an official count).
func EstimateChatTokens(chat *jsonx.Obj) int {
	total := 0
	add := func(v any) {
		if s := jsonText(v); s != "" {
			total += estimateTokens(s)
		}
	}
	if msgs, ok := chat.Get("messages").([]any); ok {
		for _, m := range msgs {
			mo, ok := asObj(m)
			if !ok {
				continue
			}
			add(mo.Get("content"))
			if calls, ok := mo.Get("tool_calls").([]any); ok {
				for _, c := range calls {
					if co, ok := asObj(c); ok {
						add(co)
					}
				}
			}
		}
	}
	if tools, ok := chat.Get("tools").([]any); ok {
		for _, t := range tools {
			add(t)
		}
	}
	return total
}

func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	cjk, n := 0, 0
	for _, r := range s {
		n++
		if (r >= 0x4e00 && r <= 0x9fff) || (r >= 0x3400 && r <= 0x4dbf) {
			cjk++
		}
	}
	other := int(math.Floor(float64(n-cjk) / 3.6))
	if other < 1 {
		other = 1
	}
	return cjk + other
}

// DefaultCached reads the best cache-hit value across every alias the
// upstreams emit.
func DefaultCached(u *jsonx.Obj) int64 {
	b, err := jsonx.Marshal(u)
	if err != nil {
		return 0
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return 0
	}
	return usage.BestCached(m)
}
