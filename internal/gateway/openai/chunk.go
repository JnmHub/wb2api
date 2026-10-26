package openai

import (
	"bytes"
	"encoding/json"
	"errors"
)

// rawObj is a JSON object whose values stay as raw bytes until inspected.
type rawObj map[string]json.RawMessage

// noiseKeys are the empty padding fields the upstream adds to every delta.
var noiseKeys = []string{"extra_fields", "refusal", "reasoning_content"}

func isFalsyRaw(v json.RawMessage) bool {
	s := bytes.TrimSpace(v)
	switch string(s) {
	case "", "null", `""`, "[]", "{}", "0", "false", "0.0":
		return true
	}
	return false
}

// chunk is one parsed upstream SSE data object.
type chunk struct {
	top     rawObj
	choices []rawObj // each element decoded one level
	deltas  []rawObj // delta object per choice (nil when absent)
}

var errUpstreamSSE = errors.New("upstream SSE error")

func upstreamSSEError(data []byte) (error, bool) {
	var top rawObj
	if err := json.Unmarshal(data, &top); err != nil || top == nil {
		return nil, false
	}
	raw, ok := top["error"]
	if !ok || isFalsyRaw(raw) {
		return nil, false
	}
	var detail struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
	}
	if json.Unmarshal(raw, &detail) != nil {
		return errUpstreamSSE, true
	}
	msg := detail.Message
	if msg == "" {
		msg = detail.Type
	}
	if msg == "" {
		msg = "upstream SSE error"
	}
	return errors.New(msg), true
}

func parseChunk(data []byte) (*chunk, bool) {
	var top rawObj
	if err := json.Unmarshal(data, &top); err != nil || top == nil {
		return nil, false
	}
	c := &chunk{top: top}
	if raw, ok := top["choices"]; ok {
		var arr []rawObj
		if err := json.Unmarshal(raw, &arr); err == nil {
			c.choices = arr
			c.deltas = make([]rawObj, len(arr))
			for i, ch := range arr {
				if dr, ok := ch["delta"]; ok {
					var d rawObj
					if json.Unmarshal(dr, &d) == nil {
						c.deltas[i] = d
					}
				}
			}
		}
	}
	return c, true
}

// outputTokens estimates visible/reasoning output in this chunk. Used only when
// stream pacing is enabled; the parsed chunk is reused instead of decoding the
// same JSON twice.
func (c *chunk) outputTokens() int {
	n := 0
	for i := range c.deltas {
		n += estimateTextTokens(c.content(i))
		n += estimateTextTokens(c.reasoning(i))
	}
	return n
}

// usage returns the chunk's usage object (raw) when present and non-empty.
func (c *chunk) usage() json.RawMessage {
	if u, ok := c.top["usage"]; ok && !isFalsyRaw(u) {
		return u
	}
	return nil
}

// content returns delta.content of choice i ("" when absent / not a string).
func (c *chunk) content(i int) string {
	if i >= len(c.deltas) || c.deltas[i] == nil {
		return ""
	}
	raw, ok := c.deltas[i]["content"]
	if !ok || isFalsyRaw(raw) {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// reasoning returns delta.reasoning_content of choice i.
func (c *chunk) reasoning(i int) string {
	if i >= len(c.deltas) || c.deltas[i] == nil {
		return ""
	}
	raw, ok := c.deltas[i]["reasoning_content"]
	if !ok || isFalsyRaw(raw) {
		return ""
	}
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

// toolCalls reports whether choice i's delta carries tool call fragments.
func (c *chunk) toolCalls(i int) bool {
	if i >= len(c.deltas) || c.deltas[i] == nil {
		return false
	}
	raw, ok := c.deltas[i]["tool_calls"]
	if !ok || isFalsyRaw(raw) {
		return false
	}
	switch b := bytes.TrimSpace(raw); string(b) {
	case "[]", "null":
		return false
	}
	return true
}

// finishReason returns choice i's finish_reason ("" when null/absent/empty).
func (c *chunk) finishReason(i int) string {
	if i >= len(c.choices) {
		return ""
	}
	raw, ok := c.choices[i]["finish_reason"]
	if !ok || isFalsyRaw(raw) {
		return ""
	}
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

// clean removes the upstream's empty padding fields and normalises an empty
// finish_reason to null. It reports whether the chunk still carries anything
// worth forwarding (the legacy clean_chunk returned "" for an empty delta
// without a finish reason).
func (c *chunk) clean() (keep bool) {
	for i, ch := range c.choices {
		if fr, ok := ch["finish_reason"]; ok && string(bytes.TrimSpace(fr)) == `""` {
			ch["finish_reason"] = json.RawMessage("null")
		}
		d := c.deltas[i]
		if d == nil {
			continue
		}
		if fc, ok := d["function_call"]; ok {
			empty := isFalsyRaw(fc)
			if !empty {
				var m map[string]any
				if json.Unmarshal(fc, &m) == nil {
					name, _ := m["name"].(string)
					empty = name == ""
				}
			}
			if empty {
				delete(d, "function_call")
			}
		}
		if tc, ok := d["tool_calls"]; ok && string(bytes.TrimSpace(tc)) == "[]" {
			delete(d, "tool_calls")
		}
		for _, k := range noiseKeys {
			if v, ok := d[k]; ok && isFalsyRaw(v) {
				delete(d, k)
			}
		}
		if len(d) == 0 && c.finishReason(i) == "" {
			return false
		}
	}
	return true
}

// removeContent drops delta.content of choice i (the text was taken over by
// the stream filter).
func (c *chunk) removeContent(i int) {
	if i < len(c.deltas) && c.deltas[i] != nil {
		delete(c.deltas[i], "content")
	}
}

// holdFinish clears finish_reason of choice i and returns the old value.
func (c *chunk) holdFinish(i int) string {
	fr := c.finishReason(i)
	if fr != "" {
		c.choices[i]["finish_reason"] = json.RawMessage("null")
	}
	return fr
}

// encode re-serialises the chunk as an SSE data line (without trailing blank line).
func (c *chunk) encode() ([]byte, error) {
	for i, ch := range c.choices {
		if c.deltas[i] != nil {
			db, err := marshalNoEscape(c.deltas[i])
			if err != nil {
				return nil, err
			}
			ch["delta"] = db
		}
	}
	if len(c.choices) > 0 {
		arr := make([]json.RawMessage, len(c.choices))
		for i, ch := range c.choices {
			b, err := marshalNoEscape(ch)
			if err != nil {
				return nil, err
			}
			arr[i] = b
		}
		ab, err := marshalNoEscape(arr)
		if err != nil {
			return nil, err
		}
		c.top["choices"] = ab
	}
	return marshalNoEscape(c.top)
}

// marshalNoEscape is json.Marshal without HTML escaping (<, > and & stay as
// they are, like Python's json.dumps(ensure_ascii=False)). RawMessage values
// are re-validated and compacted by the encoder, so they are covered too.
func marshalNoEscape(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}
