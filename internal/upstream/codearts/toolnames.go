package codearts

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"strings"

	"wb2api/internal/jsonx"
)

// MaxToolNameChars is the upstream's hard limit on tools[].function.name.
// Longer names are rejected before the request is even accepted:
//
//	InferHub.001001017.413: Function name [...] length over limit, maximum is 64
const MaxToolNameChars = 64

// toolNameHashWidths are tried in order: a short hash first, longer only when
// the short one collides with a name the client already uses.
var toolNameHashWidths = []int{7, 10, 14}

// ToolNameMap remembers the names we shortened so the response can be restored:
// the client only knows the original names.
type ToolNameMap struct {
	sent map[string]string // name sent upstream → original name
	rev  map[string]string // original name → name sent upstream
}

// Empty reports whether nothing was rewritten.
func (m *ToolNameMap) Empty() bool { return m == nil || len(m.sent) == 0 }

// SentName maps a client tool name to the one that goes on the wire.
func (m *ToolNameMap) SentName(name string) (string, bool) {
	if m == nil {
		return name, false
	}
	short, ok := m.rev[name]
	return short, ok
}

// RestoreName maps a name we sent back to the client's name.
func (m *ToolNameMap) RestoreName(name string) string {
	if m == nil {
		return name
	}
	if original, ok := m.sent[name]; ok {
		return original
	}
	return name
}

// HasSentPrefix identifies a partial wire alias in a streamed function name.
func (m *ToolNameMap) HasSentPrefix(prefix string) bool {
	if m.Empty() || prefix == "" {
		return false
	}
	for name := range m.sent {
		if strings.HasPrefix(name, prefix) && name != prefix {
			return true
		}
	}
	return false
}

// ShortenTools rewrites every tools[].function.name longer than the limit and
// returns the map needed to restore the response. tools is the decoded
// []any from the request body; it is modified in place.
//
// A name that cannot be shortened without colliding (three different names
// sharing the prefix and all three hash widths) is left alone: letting the
// upstream reject it is safer than inventing a name the client never had.
func ShortenTools(tools []any) *ToolNameMap {
	if len(tools) == 0 {
		return nil
	}
	taken := map[string]bool{}
	for _, t := range tools {
		if name := toolFunctionName(t); name != "" {
			taken[name] = true
		}
	}
	out := &ToolNameMap{sent: map[string]string{}, rev: map[string]string{}}
	for _, t := range tools {
		original := toolFunctionName(t)
		if original == "" || len([]rune(original)) <= MaxToolNameChars {
			continue
		}
		short, ok := out.rev[original]
		if !ok {
			short, ok = shortToolName(original, taken)
		}
		if !ok {
			continue
		}
		if setToolFunctionName(t, short) {
			out.sent[short] = original
			out.rev[original] = short
		}
	}
	if len(out.sent) == 0 {
		return nil
	}
	return out
}

// objGet reads a key from either body shape: the ordered jsonx object the
// gateway parses or a plain decoded map.
func objGet(v any, key string) (any, bool) {
	switch t := v.(type) {
	case map[string]any:
		x, ok := t[key]
		return x, ok
	case *jsonx.Obj:
		return t.Lookup(key)
	}
	return nil, false
}

func objStr(v any, key string) (string, bool) {
	x, ok := objGet(v, key)
	if !ok {
		return "", false
	}
	s, ok := x.(string)
	return s, ok
}

// objSet writes a key into either shape.
func objSet(v any, key string, val any) bool {
	switch t := v.(type) {
	case map[string]any:
		t[key] = val
		return true
	case *jsonx.Obj:
		t.Set(key, val)
		return true
	}
	return false
}

func cloneChatObject(v any) any {
	switch t := v.(type) {
	case *jsonx.Obj:
		return t.Clone()
	case map[string]any:
		cp := make(map[string]any, len(t))
		for k, value := range t {
			cp[k] = value
		}
		return cp
	default:
		return v
	}
}

func cloneWithFunction(v any) any {
	if fn, ok := objGet(v, "function"); ok {
		v = cloneChatObject(v)
		objSet(v, "function", cloneChatObject(fn))
	}
	return v
}

func toolFunctionName(tool any) string {
	switch t := tool.(type) {
	case map[string]any:
		fn, ok := t["function"].(map[string]any)
		if !ok {
			return ""
		}
		name, _ := fn["name"].(string)
		return name
	case *jsonx.Obj:
		// The gateway's shaped body keeps nested objects ordered.
		fn, ok := t.Obj("function")
		if !ok {
			return ""
		}
		name, _ := fn.Str("name")
		return name
	}
	return ""
}

// setToolFunctionName writes the shortened name into either body shape.
func setToolFunctionName(tool any, name string) bool {
	switch t := tool.(type) {
	case map[string]any:
		fn, ok := t["function"].(map[string]any)
		if !ok {
			return false
		}
		fn["name"] = name
		return true
	case *jsonx.Obj:
		fn, ok := t.Obj("function")
		if !ok {
			return false
		}
		fn.Set("name", name)
		return true
	}
	return false
}

// shortToolName builds "<prefix>_<hash>" that fits the limit and does not
// collide with any name already in use.
func shortToolName(original string, taken map[string]bool) (string, bool) {
	sum := sha1.Sum([]byte(original))
	hexSum := hex.EncodeToString(sum[:])
	runes := []rune(original)
	for _, width := range toolNameHashWidths {
		head := MaxToolNameChars - width - 1
		if head <= 0 {
			continue
		}
		prefix := string(runes[:min(head, len(runes))])
		candidate := prefix + "_" + hexSum[:width]
		if !taken[candidate] {
			taken[candidate] = true
			return candidate, true
		}
	}
	return "", false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// RestoreJSON rewrites the tool names inside one upstream chunk. It only
// re-serialises when a shortened name actually appears, so untouched frames keep
// their original bytes.
func (m *ToolNameMap) RestoreJSON(data []byte) ([]byte, bool) {
	if m == nil || len(m.sent) == 0 || !containsAny(data, m.sent) {
		return data, false
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return data, false
	}
	choices, _ := doc["choices"].([]any)
	changed := false
	for _, c := range choices {
		choice, ok := c.(map[string]any)
		if !ok {
			continue
		}
		for _, slot := range []string{"delta", "message"} {
			part, ok := choice[slot].(map[string]any)
			if !ok {
				continue
			}
			calls, ok := part["tool_calls"].([]any)
			if !ok {
				continue
			}
			for _, raw := range calls {
				call, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				fn, ok := call["function"].(map[string]any)
				if !ok {
					continue
				}
				name, _ := fn["name"].(string)
				if original, hit := m.sent[name]; hit {
					fn["name"] = original
					changed = true
				}
			}
		}
	}
	if !changed {
		return data, false
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return data, false
	}
	return encoded, true
}

func containsAny(data []byte, names map[string]string) bool {
	s := string(data)
	for name := range names {
		if strings.Contains(s, name) {
			return true
		}
	}
	return false
}

// RewriteHistory points earlier assistant tool calls at the shortened names:
// role, call ID, tool arguments and tool response content stay intact. The
// original request is reused by failed attempts, so every edit copies its path.
func (m *ToolNameMap) RewriteHistory(out *jsonx.Obj) {
	if m.Empty() || out == nil {
		return
	}
	msgs, ok := out.Arr("messages")
	if !ok {
		return
	}
	var changed []any
	for i, raw := range msgs {
		calls, _ := objGet(raw, "tool_calls")
		list, _ := calls.([]any)
		var rewritten []any
		for j, c := range list {
			fn, ok := objGet(c, "function")
			if !ok {
				continue
			}
			name, ok := objStr(fn, "name")
			if !ok {
				continue
			}
			if wire, hit := m.SentName(name); hit {
				if rewritten == nil {
					rewritten = append([]any(nil), list...)
				}
				call := cloneWithFunction(c)
				f, _ := objGet(call, "function")
				objSet(f, "name", wire)
				rewritten[j] = call
			}
		}
		role, _ := objStr(raw, "role")
		name, _ := objStr(raw, "name")
		wire, renamed := m.SentName(name)
		renameResult := renamed && (role == "tool" || role == "function")
		if rewritten == nil && !renameResult {
			continue
		}
		if changed == nil {
			changed = append([]any(nil), msgs...)
		}
		msg := cloneChatObject(raw)
		if rewritten != nil {
			objSet(msg, "tool_calls", rewritten)
		}
		if renameResult {
			objSet(msg, "name", wire)
		}
		changed[i] = msg
	}
	if changed != nil {
		out.Set("messages", changed)
	}
}
