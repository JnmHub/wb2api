// Package usageview prepares token usage for client responses.
package usageview

import (
	"encoding/json"
	"math"
	"strconv"

	"wb2api/internal/jsonx"
)

// WithOriginalPromptObject is the object form used by protocol converters.
func WithOriginalPromptObject(usage *jsonx.Obj, removedPromptTokens int) *jsonx.Obj {
	if usage == nil || removedPromptTokens <= 0 {
		return usage
	}
	raw, err := jsonx.Marshal(usage)
	if err != nil {
		return usage
	}
	adjusted, err := jsonx.UnmarshalObj(WithOriginalPrompt(raw, removedPromptTokens))
	if err != nil {
		return usage
	}
	return adjusted
}

// WithOriginalPrompt adds the estimated tokens removed during history trimming
// to the upstream prompt count for display. Keep the upstream usage for storage
// and billing so they reflect what the provider actually processed.
func WithOriginalPrompt(usage json.RawMessage, removedPromptTokens int) json.RawMessage {
	if removedPromptTokens <= 0 || len(usage) == 0 {
		return usage
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(usage, &fields); err != nil {
		return usage
	}
	removed := int64(removedPromptTokens)
	prompt, ok := tokenCount(fields["prompt_tokens"])
	if !ok || prompt > math.MaxInt64-removed {
		return usage
	}
	prompt += removed
	if completion, ok := tokenCount(fields["completion_tokens"]); ok {
		if prompt > math.MaxInt64-completion {
			return usage
		}
		fields["total_tokens"] = rawCount(prompt + completion)
	} else if total, ok := tokenCount(fields["total_tokens"]); ok {
		if total > math.MaxInt64-removed {
			return usage
		}
		fields["total_tokens"] = rawCount(total + removed)
	}
	fields["prompt_tokens"] = rawCount(prompt)
	fields["input_tokens_estimated"] = json.RawMessage("true")
	adjusted, err := json.Marshal(fields)
	if err != nil {
		return usage
	}
	return adjusted
}

func tokenCount(value json.RawMessage) (int64, bool) {
	count, err := strconv.ParseInt(string(value), 10, 64)
	return count, err == nil && count >= 0
}

func rawCount(value int64) json.RawMessage {
	return strconv.AppendInt(nil, value, 10)
}
