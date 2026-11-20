package codearts

import "wb2api/internal/jsonx"

// Production requests near 9.5 MiB have succeeded on CodeArts, while requests
// over 10 MiB have hit the inference gateway's size wall. Trim replayed thoughts
// only for large bodies and let the upstream handle the resulting request size.
const reasoningTrimThreshold = 10 << 20

// Like Agent2Providers, remove historical assistant reasoning before signing.
// Stop at the current user turn so in-progress tool calls can continue their
// reasoning. Copy the edited paths because retries reuse the incoming messages.
func trimHistoricalReasoning(out *jsonx.Obj) bool {
	messages, _ := out.Arr("messages")
	lastUser := len(messages) - 1
	for lastUser >= 0 && !startsUserTurn(messages[lastUser]) {
		lastUser--
	}
	if lastUser <= 0 {
		return false
	}
	var trimmed []any
	for i, msg := range messages[:lastUser] {
		if role, _ := objStr(msg, "role"); role != "assistant" {
			continue
		}
		if prefix, _ := objGet(msg, "prefix"); prefix == true {
			continue
		}
		if next, changed := trimAssistantReasoning(msg); changed {
			if trimmed == nil {
				trimmed = append([]any(nil), messages...)
			}
			trimmed[i] = next
		}
	}
	if trimmed == nil {
		return false
	}
	out.Set("messages", trimmed)
	return true
}

func startsUserTurn(msg any) bool {
	if role, _ := objStr(msg, "role"); role != "user" {
		return false
	}
	content, _ := objGet(msg, "content")
	blocks, ok := content.([]any)
	if !ok || len(blocks) == 0 {
		return true
	}
	// Some clients place tool returns in user-role blocks. A message made up of
	// those returns continues the current turn.
	for _, block := range blocks {
		if typ, _ := objStr(block, "type"); typ != "tool_result" {
			return true
		}
	}
	return false
}

func trimAssistantReasoning(msg any) (any, bool) {
	var next any
	for _, key := range []string{"reasoning_content", "reasoning"} {
		if text, _ := objStr(msg, key); text != "" {
			if next == nil {
				next = cloneChatObject(msg)
			}
			switch obj := next.(type) {
			case *jsonx.Obj:
				obj.Delete(key)
			case map[string]any:
				delete(obj, key)
			}
		}
	}
	content, _ := objGet(msg, "content")
	blocks, _ := content.([]any)
	var kept []any
	for i, block := range blocks {
		typ, _ := objStr(block, "type")
		_, signed := objGet(block, "signature")
		if typ == "thinking" && !signed {
			if kept == nil {
				kept = make([]any, 0, len(blocks)-1)
				kept = append(kept, blocks[:i]...)
			}
			continue
		}
		if kept != nil {
			kept = append(kept, block)
		}
	}
	if kept != nil {
		if next == nil {
			next = cloneChatObject(msg)
		}
		if len(kept) == 0 {
			objSet(next, "content", "")
		} else {
			objSet(next, "content", kept)
		}
	}
	return next, next != nil
}
