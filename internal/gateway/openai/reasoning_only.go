package openai

// reasoningOnlyDetail is what a client sees when the upstream answered with
// thinking but no content and no tool call. Such a reply used to be forwarded as
// a successful empty answer, which made clients show a blank turn.
const reasoningOnlyDetail = "upstream returned reasoning without content"

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
