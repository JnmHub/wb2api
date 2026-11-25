package chathistory

import (
	"unicode"

	"wb2api/internal/jsonx"
)

// Estimate the delta at the final safe cut. Retained system/developer
// messages don't contribute, even if they precede the cut.
func removedTokens(messages []any, restoreName func(string) string) int {
	var total int
	for _, msg := range messages {
		role, _ := objStr(msg, "role")
		if role == "system" || role == "developer" {
			continue
		}
		var text promptText
		content, _ := objGet(msg, "content")
		text.addContent(content, restoreName)
		for _, field := range []string{"name", "tool_call_id", "reasoning_content", "refusal"} {
			value, _ := objStr(msg, field)
			if field == "name" && restoreName != nil {
				value = restoreName(value)
			}
			text.add(value)
		}
		if fn, ok := objGet(msg, "function_call"); ok {
			text.addFunction(fn, "arguments", restoreName)
		}
		raw, _ := objGet(msg, "tool_calls")
		calls, _ := raw.([]any)
		for _, call := range calls {
			id, _ := objStr(call, "id")
			text.add(id)
			fn, _ := objGet(call, "function")
			text.addFunction(fn, "arguments", restoreName)
		}
		const messageFramingTokens = 3
		total += messageFramingTokens + text.tokens()
	}
	return total
}

// Match the gateway's rough text estimate, one token per Han character and
// one per 3.6 remaining characters. Provider tokenizers may differ.
type promptText struct{ han, other int }

func (p *promptText) add(s string) {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			p.han++
		} else {
			p.other++
		}
	}
}

func (p *promptText) tokens() int {
	if p.other == 0 {
		return p.han
	}
	return p.han + max(1, int(float64(p.other)/3.6))
}

func (p *promptText) addContent(content any, restoreName func(string) string) {
	switch value := content.(type) {
	case string:
		p.add(value)
	case []any:
		for _, block := range value {
			kind, _ := objStr(block, "type")
			switch kind {
			case "tool_use", "server_tool_use":
				id, _ := objStr(block, "id")
				p.add(id)
				p.addFunction(block, "input", restoreName)
			case "tool_result":
				id, _ := objStr(block, "tool_use_id")
				p.add(id)
				value, _ := objGet(block, "content")
				p.addContent(value, restoreName)
			default:
				// Media block URLs/base64 are transport representations, not
				// text prompt tokens. Their provider-specific cost is unknown.
				if text, ok := objStr(block, "text"); ok {
					p.add(text)
				} else if text, ok := block.(string); ok {
					p.add(text)
				}
			}
		}
	}
}

func (p *promptText) addFunction(fn any, argsKey string, restoreName func(string) string) {
	name, _ := objStr(fn, "name")
	if restoreName != nil {
		name = restoreName(name)
	}
	p.add(name)
	if args, ok := objGet(fn, argsKey); ok && args != nil {
		if s, ok := args.(string); ok {
			p.add(s)
		} else if b, err := jsonx.Marshal(args); err == nil {
			p.add(string(b))
		}
	}
}
