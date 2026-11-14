package openai

import "strings"

// reasoningOnlyDetail is what a client sees when the upstream answered with
// thinking but no content and no tool call. Such a reply used to be forwarded as
// a successful empty answer, which made clients show a blank turn.
const reasoningOnlyDetail = "upstream returned reasoning without content"

// reasoningOnlyBudgetHint explains the most common cause: the thinking models
// this gateway serves (most of the CN catalogue is thinking-by-default) spend
// the whole max_tokens budget on reasoning, so the upstream closes with
// finish_reason=length and never writes an answer. Without the hint the bare
// message reads like a gateway fault instead of a budget the client controls.
const reasoningOnlyBudgetHint = " (finish_reason=length: the max_tokens budget was consumed by thinking; " +
	"raise max_tokens, lower reasoning_effort, or use a model that can disable thinking)"

// reasoningOnlyMessage names the cause when the upstream finish reason is
// known. An empty finish reason keeps the plain wording.
func reasoningOnlyMessage(finish string) string {
	if strings.EqualFold(strings.TrimSpace(finish), "length") {
		return reasoningOnlyDetail + reasoningOnlyBudgetHint
	}
	return reasoningOnlyDetail
}

// chunkSignals reports what one upstream chat-completions chunk carries. The
// Responses and Messages relays read the same upstream dialect, so the same
// parser answers "did the model actually say anything?" for all three.
func chunkSignals(data []byte) (content, reasoning, calls bool) {
	c, ok := parseChunk(data)
	if !ok || c == nil {
		return false, false, false
	}
	for i := range c.choices {
		if c.content(i) != "" {
			content = true
		}
		if c.reasoning(i) != "" {
			reasoning = true
		}
		if c.toolCalls(i) {
			calls = true
		}
	}
	return content, reasoning, calls
}
