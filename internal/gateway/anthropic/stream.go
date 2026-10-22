package anthropic

import (
	"sort"
	"strings"

	"wb2api/internal/jsonx"
)

// Frame is one server-sent event, ready to write.
type Frame []byte

// frame is anthropic_sse_frame: the event name is also stored as "type" after
// the payload's own keys, and the JSON is compact.
func frame(event string, payload *jsonx.Obj) Frame {
	body := jsonx.NewObj()
	if payload != nil {
		payload.Each(func(k string, v any) { body.Set(k, v) })
	}
	body.Set("type", event)
	b, _ := jsonx.Marshal(body)
	return Frame("event: " + event + "\ndata: " + string(b) + "\n\n")
}

// Frame builds a one-off event (used for errors).
func ErrorFrame(errType, message string) Frame {
	return frame("error", obj("error", obj("type", errType, "message", message)))
}

type toolEntry struct {
	block  int
	id     string
	name   string
	opened bool
}

// Stream translates an upstream chat-completions stream into Messages events.
type Stream struct {
	env    Env
	cached Cached
	model  string

	started, done, failed  bool
	id, stop               string
	thinkingOpen, textOpen bool
	thinkingIdx, textIdx   int
	next                   int
	tools                  map[int]*toolEntry
	toolOrder              []int
	usage                  *jsonx.Obj
}

// NewStream starts a translation.
func NewStream(model string, env Env, cached Cached) *Stream {
	return &Stream{env: env, cached: cached, model: model, stop: "end_turn", tools: map[int]*toolEntry{}}
}

// Usage is the raw upstream usage block (nil when none was seen).
func (s *Stream) Usage() *jsonx.Obj { return s.usage }

// Failed reports whether an upstream error envelope ended the stream.
func (s *Stream) Failed() bool { return s.failed }

func (s *Stream) ensureStart(chunk *jsonx.Obj) []Frame {
	if s.started {
		return nil
	}
	s.started = true
	if chunk != nil {
		s.id = text(chunk.Get("id"))
		if m := text(chunk.Get("model")); m != "" {
			s.model = m
		}
	}
	return []Frame{frame("message_start", obj("message", obj(
		"id", messageID(s.env, s.id), "type", "message", "role", "assistant", "content", []any{},
		"model", s.model, "stop_reason", nil, "stop_sequence", nil,
		"usage", obj("input_tokens", num(0), "output_tokens", num(0)))))}
}

func (s *Stream) ensureThinking() []Frame {
	if s.thinkingOpen {
		return nil
	}
	s.thinkingIdx = s.next
	s.next++
	s.thinkingOpen = true
	return []Frame{frame("content_block_start", obj("index", num(int64(s.thinkingIdx)),
		"content_block", obj("type", "thinking", "thinking", "")))}
}

func (s *Stream) ensureText() []Frame {
	if s.textOpen {
		return nil
	}
	var out []Frame
	if s.thinkingOpen {
		s.thinkingOpen = false
		out = append(out, frame("content_block_stop", obj("index", num(int64(s.thinkingIdx)))))
	}
	s.textIdx = s.next
	s.next++
	s.textOpen = true
	return append(out, frame("content_block_start", obj("index", num(int64(s.textIdx)),
		"content_block", obj("type", "text", "text", ""))))
}

// Line consumes one raw upstream SSE line.
func (s *Stream) Line(line []byte) []Frame {
	if s.done || s.failed {
		return nil
	}
	data := strings.TrimSpace(string(line))
	if data == "" || data[0] == ':' {
		return nil
	}
	for strings.HasPrefix(data, "data:") {
		data = strings.TrimSpace(data[5:])
	}
	if data == "" || data[0] == ':' || data == "[DONE]" {
		return nil
	}
	chunk, err := jsonx.UnmarshalObj([]byte(data))
	if err != nil {
		return nil
	}
	return s.Chunk(chunk)
}

// Chunk consumes one parsed upstream chunk.
func (s *Stream) Chunk(chunk *jsonx.Obj) []Frame {
	var out []Frame
	e := chunk.Get("error")
	_, objectError := asObj(e)
	if objectError || tr(e) {
		s.failed = true
		// Upstream details may contain credentials or internal endpoints.
		return []Frame{ErrorFrame(ErrorType(502), "upstream stream aborted")}
	}
	out = append(out, s.ensureStart(chunk)...)
	// usage is read before the choices guard: the final frame carries both usage
	// and finish_reason (and no delta)
	if u, ok := asObj(chunk.Get("usage")); ok && u.Len() > 0 {
		s.usage = u
	}
	choices, _ := chunk.Get("choices").([]any)
	if len(choices) == 0 {
		return out
	}
	choice, _ := asObj(choices[0])
	if choice == nil {
		choice = jsonx.NewObj()
	}
	delta, _ := asObj(choice.Get("delta"))
	if delta == nil {
		delta = jsonx.NewObj()
	}
	if r, ok := delta.Get("reasoning_content").(string); ok && r != "" {
		out = append(out, s.ensureThinking()...)
		out = append(out, frame("content_block_delta", obj("index", num(int64(s.thinkingIdx)),
			"delta", obj("type", "thinking_delta", "thinking", r))))
	}
	if t, ok := delta.Get("content").(string); ok && t != "" {
		out = append(out, s.ensureText()...)
		out = append(out, frame("content_block_delta", obj("index", num(int64(s.textIdx)),
			"delta", obj("type", "text_delta", "text", t))))
	}
	calls, _ := delta.Get("tool_calls").([]any)
	for _, raw := range calls {
		call, ok := asObj(raw)
		if !ok {
			continue
		}
		idx := int(toInt(call.Get("index")))
		e := s.tools[idx]
		if e == nil {
			e = &toolEntry{}
			s.tools[idx] = e
			s.toolOrder = append(s.toolOrder, idx)
		}
		if tr(call.Get("id")) {
			e.id = text(call.Get("id"))
		}
		fn, _ := asObj(call.Get("function"))
		if fn == nil {
			fn = jsonx.NewObj()
		}
		if tr(fn.Get("name")) {
			e.name = text(fn.Get("name"))
		}
		args := ""
		if a := fn.Get("arguments"); a != nil {
			args = text(a)
		}
		if !e.opened && (e.id != "" || e.name != "" || args != "") {
			if s.thinkingOpen {
				s.thinkingOpen = false
				out = append(out, frame("content_block_stop", obj("index", num(int64(s.thinkingIdx)))))
			}
			if s.textOpen {
				s.textOpen = false
				out = append(out, frame("content_block_stop", obj("index", num(int64(s.textIdx)))))
			}
			e.opened = true
			e.block = s.next
			s.next++
			if e.id == "" {
				e.id = "toolu_" + itoa(idx)
			}
			out = append(out, frame("content_block_start", obj("index", num(int64(e.block)),
				"content_block", obj("type", "tool_use", "id", e.id, "name", e.name, "input", jsonx.NewObj()))))
		}
		if e.opened && args != "" {
			out = append(out, frame("content_block_delta", obj("index", num(int64(e.block)),
				"delta", obj("type", "input_json_delta", "partial_json", args))))
		}
	}
	if tr(choice.Get("finish_reason")) {
		s.stop = stopReason(choice.Get("finish_reason"))
	}
	return out
}

func itoa(n int) string { return jsonx.PyStr(num(int64(n))) }

// Finish closes every open block and emits message_delta / message_stop.
func (s *Stream) Finish() []Frame {
	if s.failed {
		return nil
	}
	var out []Frame
	if !s.started {
		out = append(out, s.ensureStart(nil)...)
	}
	if s.thinkingOpen {
		s.thinkingOpen = false
		out = append(out, frame("content_block_stop", obj("index", num(int64(s.thinkingIdx)))))
	}
	if s.textOpen {
		s.textOpen = false
		out = append(out, frame("content_block_stop", obj("index", num(int64(s.textIdx)))))
	}
	order := append([]int(nil), s.toolOrder...)
	_ = sort.Ints
	for _, idx := range order {
		if e := s.tools[idx]; e.opened {
			out = append(out, frame("content_block_stop", obj("index", num(int64(e.block)))))
		}
	}
	s.done = true
	out = append(out,
		frame("message_delta", obj("delta", obj("stop_reason", s.stop, "stop_sequence", nil), "usage", usageOf(s.usage, s.cached))),
		frame("message_stop", jsonx.NewObj()))
	return out
}
