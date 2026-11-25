// Package chathistory trims old turns toward an outbound byte budget.
package chathistory

import "wb2api/internal/jsonx"

// Apply trims complete old turns from an owned, normalized body. If the
// retained messages still exceed the target, it returns the smallest safe
// body. It also estimates the input tokens removed by the selected cut.
// encoded must be the current compact encoding of out. restoreName may be nil.
func Apply(out *jsonx.Obj, encoded []byte, limit int, restoreName func(string) string) ([]byte, int, error) {
	if limit <= 0 || len(encoded) <= limit {
		return encoded, 0, nil
	}
	messages, _ := out.Arr("messages")
	turns, crossing := historyBoundaries(messages, restoreName)
	size := len(encoded)
	kept := make([]any, 0, len(messages))
	start, safeCut, safeKept := 0, 0, 0
	for _, cut := range turns {
		for _, msg := range messages[start:cut] {
			role, _ := objStr(msg, "role")
			if role == "system" || role == "developer" {
				kept = append(kept, msg)
				continue
			}
			msgBytes, err := jsonx.Marshal(msg)
			if err != nil {
				return nil, 0, err
			}
			// At least the current user message remains. Every removed
			// element eliminates its exact JSON bytes and one comma.
			size -= len(msgBytes) + 1
		}
		start = cut
		if crossing[cut] == 0 {
			safeCut, safeKept = cut, len(kept)
			if size <= limit {
				break
			}
		}
	}
	if safeCut == safeKept {
		return encoded, 0, nil
	}
	out.Set("messages", append(kept[:safeKept], messages[safeCut:]...))
	body, err := jsonx.Marshal(out)
	if err != nil {
		return nil, 0, err
	}
	return body, removedTokens(messages[:safeCut], restoreName), nil
}

type historyToolRef struct{ protocol, id string }

// historyBoundaries marks cuts that would separate a removed tool call from
// a retained result, including mixed user/tool-result blocks.
func historyBoundaries(messages []any, restoreName func(string) string) ([]int, []int) {
	var turns []int
	delta := make([]int, len(messages))
	calls := make(map[historyToolRef]int)
	seenAssistant, followsToolResult := false, false
	for i, msg := range messages {
		role, _ := objStr(msg, "role")
		remember := func(protocol, id string) {
			if id != "" {
				calls[historyToolRef{protocol, id}] = i
			}
		}
		result := func(protocol, id string) {
			if start, ok := calls[historyToolRef{protocol, id}]; ok && start < i {
				delta[start+1]++
				if i+1 < len(delta) {
					delta[i+1]--
				}
			}
		}
		switch role {
		case "assistant":
			seenAssistant = true
			raw, _ := objGet(msg, "tool_calls")
			list, _ := raw.([]any)
			for _, call := range list {
				id, _ := objStr(call, "id")
				remember("chat", id)
			}
			if fn, ok := objGet(msg, "function_call"); ok {
				name, _ := objStr(fn, "name")
				if restoreName != nil {
					name = restoreName(name)
				}
				remember("legacy", name)
			}
		case "tool":
			id, _ := objStr(msg, "tool_call_id")
			result("chat", id)
		case "function":
			name, _ := objStr(msg, "name")
			if restoreName != nil {
				name = restoreName(name)
			}
			result("legacy", name)
		}
		hasToolResult := false
		if role == "assistant" || role == "user" {
			content, _ := objGet(msg, "content")
			blocks, _ := content.([]any)
			for _, block := range blocks {
				kind, _ := objStr(block, "type")
				switch {
				case role == "assistant" && kind == "tool_use":
					id, _ := objStr(block, "id")
					remember("messages", id)
				case role == "user" && kind == "tool_result":
					id, _ := objStr(block, "tool_use_id")
					result("messages", id)
					hasToolResult = true
				}
			}
		}
		// Messages conversion may split a single mixed user message into
		// role=tool followed by role=user. Keep that text with its result.
		if role == "user" && !hasToolResult && !followsToolResult && (len(turns) == 0 || seenAssistant) {
			turns = append(turns, i)
			seenAssistant = false
		}
		if role != "system" && role != "developer" {
			followsToolResult = role == "tool" || role == "function" || hasToolResult
		}
	}
	for i := 1; i < len(delta); i++ {
		delta[i] += delta[i-1]
	}
	return turns, delta
}

func objGet(v any, key string) (any, bool) {
	switch obj := v.(type) {
	case *jsonx.Obj:
		if obj != nil {
			return obj.Lookup(key)
		}
	case map[string]any:
		value, ok := obj[key]
		return value, ok
	}
	return nil, false
}

func objStr(v any, key string) (string, bool) {
	value, _ := objGet(v, key)
	s, ok := value.(string)
	return s, ok
}
