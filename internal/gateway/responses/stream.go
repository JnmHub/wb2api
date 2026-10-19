package responses

import (
	"sort"
	"strings"
	"unicode/utf8"

	"wb2api/internal/gateway/leak"
	"wb2api/internal/jsonx"
)

// Frame is one server-sent event, ready to write.
type Frame []byte

type toolEntry struct {
	outputIndex int
	id          string
	name        string
	arguments   string
	custom      bool
	itemID      string
}

// Stream translates an upstream chat-completions stream into Responses events.
// Feed it every parsed upstream chunk with Chunk, then call Finish.
type Stream struct {
	model string
	info  Info
	env   Env

	respID, msgID, rsID string
	created             int64
	seq                 int

	textParts, reasonParts []string
	outputs                []*jsonx.Obj
	reasonIndex, msgIndex  int // -1 = not started
	finish                 string
	usage                  *jsonx.Obj
	tools                  map[int]*toolEntry
	textBuffer             string
	dsmlCalls              []dsmlCall
	sawDone                bool
	estimatedUsage         bool
}

// NewStream starts a translation; Start returns the opening events.
func NewStream(model string, info Info, env Env) *Stream {
	s := &Stream{model: model, info: info, env: env, reasonIndex: -1, msgIndex: -1,
		finish: "stop", tools: map[int]*toolEntry{}}
	s.respID, s.msgID, s.rsID = env.id("resp_"), env.id("msg_"), env.id("rs_")
	s.created = env.now()
	return s
}

// Usage is the final upstream usage block (nil when none was seen), also
// estimated locally when the upstream reported none.
func (s *Stream) Usage() *jsonx.Obj { return s.usage }

// Estimated reports whether Usage was estimated locally.
func (s *Stream) Estimated() bool { return s.estimatedUsage }

func (s *Stream) respObj(status string) *jsonx.Obj {
	out := []any{}
	for _, o := range s.outputs {
		if o != nil {
			out = append(out, o)
		}
	}
	tools := s.info.Tools
	if tools == nil {
		tools = []any{}
	}
	obj := obj("id", s.respID, "object", "response", "created_at", num(s.created), "status", status,
		"model", s.model, "output", out, "output_text", strings.Join(s.textParts, ""),
		"parallel_tool_calls", s.info.ParallelToolCalls, "tool_choice", s.info.ToolChoice,
		"tools", tools, "metadata", jsonx.NewObj())
	if u := Usage(s.usage); u != nil {
		obj.Set("usage", u)
	}
	applyNamespaceToCalls(out, s.info.Namespaces)
	return obj
}

// stripEmptyFC removes empty function_call placeholders and empty tool_calls
// lists anywhere in an event (the legacy clean_responses_frame).
func stripEmptyFC(v any) {
	switch t := v.(type) {
	case *jsonx.Obj:
		if fc, ok := asObj(t.Get("function_call")); ok && !tr(fc.Get("name")) {
			t.Delete("function_call")
		}
		if l, ok := t.Get("tool_calls").([]any); ok && len(l) == 0 {
			t.Delete("tool_calls")
		}
		t.Each(func(_ string, x any) { stripEmptyFC(x) })
	case []any:
		for _, x := range t {
			stripEmptyFC(x)
		}
	}
}

func (s *Stream) ev(etype string, payload *jsonx.Obj) Frame {
	s.seq++
	data := obj("type", etype, "sequence_number", num(int64(s.seq)))
	payload.Each(func(k string, v any) { data.Set(k, v) })
	stripEmptyFC(data)
	return Frame("event: " + etype + "\ndata: " + dumps(data) + "\n\n")
}

func (s *Stream) reasonItem(status string) *jsonx.Obj {
	return obj("id", s.rsID, "type", "reasoning", "status", status,
		"summary", []any{obj("type", "summary_text", "text", strings.Join(s.reasonParts, ""))})
}

func (s *Stream) msgItem(status string) *jsonx.Obj {
	item := obj("id", s.msgID, "type", "message", "status", status, "role", "assistant", "content", []any{})
	if len(s.textParts) > 0 {
		item.Set("content", []any{obj("type", "output_text", "text", strings.Join(s.textParts, ""), "annotations", []any{})})
	}
	return item
}

func (s *Stream) addOutput(o *jsonx.Obj) int {
	s.outputs = append(s.outputs, o)
	return len(s.outputs) - 1
}

// WebCall emits the client-visible cards for a locally executed web tool. The
// call itself is not added to the final output (the answer message is), matching
// the legacy event stream where cards are streamed as they happen.
func (s *Stream) WebCall(id, kind, query string) []Frame {
	idx := len(s.outputs)
	item := obj("id", id, "type", "web_search_call", "status", "in_progress")
	out := []Frame{
		s.ev("response.output_item.added", obj("output_index", num(int64(idx)), "item", item)),
		s.ev("response.web_search_call.in_progress", obj("item_id", id, "output_index", num(int64(idx)))),
	}
	action := obj("type", "search", "query", query)
	if kind == "web_fetch" {
		out = append(out, s.ev("response.web_search_call.searching", obj("item_id", id, "output_index", num(int64(idx)))))
		action = obj("type", "open_page", "url", query)
	} else {
		out = append(out, s.ev("response.web_search_call.searching", obj("item_id", id, "output_index", num(int64(idx)))))
	}
	done := obj("id", id, "type", "web_search_call", "status", "completed", "action", action)
	out = append(out,
		s.ev("response.output_item.done", obj("output_index", num(int64(idx)), "item", done)),
		s.ev("response.web_search_call.completed", obj("item_id", id, "output_index", num(int64(idx)))),
	)
	return out
}

// Start returns response.created and response.in_progress.
func (s *Stream) Start() []Frame {
	return []Frame{
		s.ev("response.created", obj("response", s.respObj("in_progress"))),
		s.ev("response.in_progress", obj("response", s.respObj("in_progress"))),
	}
}

func (s *Stream) closeReasoning(out *[]Frame) {
	full := strings.Join(s.reasonParts, "")
	*out = append(*out,
		s.ev("response.reasoning_summary_text.done", obj("item_id", s.rsID, "output_index", num(int64(s.reasonIndex)), "summary_index", num(0), "text", full)),
		s.ev("response.reasoning_summary_part.done", obj("item_id", s.rsID, "output_index", num(int64(s.reasonIndex)), "summary_index", num(0),
			"part", obj("type", "summary_text", "text", full))))
	s.outputs[s.reasonIndex] = s.reasonItem("completed")
	*out = append(*out, s.ev("response.output_item.done", obj("output_index", num(int64(s.reasonIndex)), "item", s.outputs[s.reasonIndex])))
}

func (s *Stream) textDelta(out *[]Frame, text string) {
	s.textParts = append(s.textParts, text)
	*out = append(*out, s.ev("response.output_text.delta", obj("item_id", s.msgID, "output_index", num(int64(s.msgIndex)),
		"content_index", num(0), "delta", text)))
}

func toolIndex(tc *jsonx.Obj) int {
	if v, ok := tc.Lookup("index"); ok && v != nil {
		return int(numOf(v))
	}
	return 0
}

// Chunk consumes one upstream chat.completion.chunk.
func (s *Stream) Chunk(chunk *jsonx.Obj) []Frame {
	var out []Frame
	if uo, ok := asObj(chunk.Get("usage")); ok && uo.Len() > 0 {
		if s.usage == nil || numOf(uo.Get("total_tokens")) >= numOf(s.usage.Get("total_tokens")) {
			s.usage = uo
		}
	}
	choices, _ := chunk.Get("choices").([]any)
	for _, rawChoice := range choices {
		choice, ok := asObj(rawChoice)
		if !ok {
			continue
		}
		delta, _ := asObj(choice.Get("delta"))
		if delta == nil {
			delta = jsonx.NewObj()
		}
		if piece := jsonx.PyStrIfTruthy(delta.Get("reasoning_content")); piece != "" {
			if s.reasonIndex < 0 {
				s.reasonIndex = s.addOutput(nil)
				out = append(out,
					s.ev("response.output_item.added", obj("output_index", num(int64(s.reasonIndex)), "item", s.reasonItem("in_progress"))),
					s.ev("response.reasoning_summary_part.added", obj("item_id", s.rsID, "output_index", num(int64(s.reasonIndex)),
						"summary_index", num(0), "part", obj("type", "summary_text", "text", ""))))
			}
			s.reasonParts = append(s.reasonParts, piece)
			out = append(out, s.ev("response.reasoning_summary_text.delta", obj("item_id", s.rsID, "output_index", num(int64(s.reasonIndex)),
				"summary_index", num(0), "delta", piece)))
		}
		tcs, _ := delta.Get("tool_calls").([]any)
		for _, rawTC := range tcs {
			tc, ok := asObj(rawTC)
			if !ok {
				continue
			}
			idx := toolIndex(tc)
			fn, _ := asObj(tc.Get("function"))
			if fn == nil {
				fn = jsonx.NewObj()
			}
			fnName := jsonx.PyStrIfTruthy(fn.Get("name"))
			fnArgs := jsonx.PyStrIfTruthy(fn.Get("arguments"))
			callID := jsonx.PyStrIfTruthy(tc.Get("id"))
			entry := s.tools[idx]
			if entry == nil {
				outIdx := s.addOutput(nil)
				if callID == "" {
					callID = s.env.id("call_")
				}
				custom := fnName != "" && s.info.CustomNames[fnName]
				prefix := "fc_"
				if custom {
					prefix = "ctc_"
				}
				entry = &toolEntry{outputIndex: outIdx, id: callID, name: fnName, arguments: fnArgs, custom: custom, itemID: s.env.id(prefix)}
				s.tools[idx] = entry
				item := obj("id", entry.itemID, "status", "in_progress", "call_id", callID, "name", fnName)
				if custom {
					item.Set("type", "custom_tool_call")
					item.Set("input", "")
				} else {
					item.Set("type", "function_call")
					item.Set("arguments", "")
				}
				stampNamespace(item, s.info.Namespaces)
				out = append(out, s.ev("response.output_item.added", obj("output_index", num(int64(outIdx)), "item", item)))
			} else {
				if fnName != "" && entry.name == "" {
					entry.name = fnName
					if s.info.CustomNames[fnName] {
						entry.custom = true
					}
				}
				if fnArgs != "" {
					entry.arguments += fnArgs
					etype := "response.function_call_arguments.delta"
					if entry.custom {
						etype = "response.custom_tool_call_input.delta"
					}
					out = append(out, s.ev(etype, obj("output_index", num(int64(entry.outputIndex)), "item_id", entry.itemID,
						"call_id", entry.id, "delta", fnArgs)))
				}
			}
		}
		if piece := jsonx.PyStrIfTruthy(delta.Get("content")); piece != "" {
			if s.msgIndex < 0 {
				if s.reasonIndex >= 0 {
					s.closeReasoning(&out)
				}
				s.msgIndex = s.addOutput(nil)
				out = append(out,
					s.ev("response.output_item.added", obj("output_index", num(int64(s.msgIndex)),
						"item", obj("id", s.msgID, "type", "message", "status", "in_progress", "role", "assistant", "content", []any{}))),
					s.ev("response.content_part.added", obj("item_id", s.msgID, "output_index", num(int64(s.msgIndex)), "content_index", num(0),
						"part", obj("type", "output_text", "text", "", "annotations", []any{}))))
			}
			s.textBuffer += piece
			s.drainText(&out)
		}
		if fr := choice.Get("finish_reason"); tr(fr) {
			s.finish = jsonx.PyStr(fr)
		}
	}
	return out
}

// drainText forwards buffered text, withholding anything that may be the start
// of a printed DSML tool-call block. (Like the original, a held-back "<" at the
// very start of the buffer still emits an empty delta.)
func (s *Stream) drainText(out *[]Frame) {
	for s.textBuffer != "" {
		idx := strings.IndexByte(s.textBuffer, '<')
		if idx < 0 {
			s.textDelta(out, s.textBuffer)
			s.textBuffer = ""
			return
		}
		if bs, be, ok := leak.FindDSMLBlock(s.textBuffer); ok && bs == idx {
			if idx > 0 {
				s.textDelta(out, s.textBuffer[:idx])
			}
			calls, _ := s.env.parseDSML(s.textBuffer[bs:be])
			s.dsmlCalls = append(s.dsmlCalls, calls...)
			s.textBuffer = s.textBuffer[be:]
			continue
		}
		cand := runeSlice(s.textBuffer, idx, 30)
		isCand := strings.Contains(cand, "DSML") ||
			(utf8.RuneCountInString(cand) < 10 && !strings.ContainsAny(cand, " \t\n>"))
		if isCand {
			s.textDelta(out, s.textBuffer[:idx])
			s.textBuffer = s.textBuffer[idx:]
			return
		}
		if next := strings.IndexByte(s.textBuffer[idx+1:], '<'); next >= 0 {
			flush := idx + 1 + next
			s.textDelta(out, s.textBuffer[:flush])
			s.textBuffer = s.textBuffer[flush:]
		} else {
			s.textDelta(out, s.textBuffer)
			s.textBuffer = ""
			return
		}
	}
}

// SawDone records that the upstream sent its [DONE] marker.
func (s *Stream) SawDone() { s.sawDone = true }

// Finish closes the stream: it drops tool calls whose arguments were cut off,
// then emits the remaining items and response.completed.
func (s *Stream) Finish() []Frame {
	dropped := 0
	if len(s.tools) > 0 && (s.finish == "length" || !s.sawDone) {
		for _, idx := range sortedKeys(s.tools) {
			if isTruncatedArguments(s.tools[idx].arguments) {
				delete(s.tools, idx)
				dropped++
			}
		}
	}
	var out []Frame
	if s.reasonIndex >= 0 && s.outputs[s.reasonIndex] == nil {
		s.closeReasoning(&out)
	}
	for _, idx := range sortedKeys(s.tools) {
		e := s.tools[idx]
		var item *jsonx.Obj
		if e.custom {
			in := unwrapCustomInput(e.arguments)
			out = append(out, s.ev("response.custom_tool_call_input.done", obj("output_index", num(int64(e.outputIndex)),
				"item_id", e.itemID, "call_id", e.id, "input", in)))
			item = obj("id", e.itemID, "type", "custom_tool_call", "status", "completed", "call_id", e.id, "name", e.name, "input", unwrapCustomInput(e.arguments))
		} else {
			out = append(out, s.ev("response.function_call_arguments.done", obj("output_index", num(int64(e.outputIndex)),
				"item_id", e.itemID, "call_id", e.id, "arguments", e.arguments)))
			item = obj("id", e.itemID, "type", "function_call", "status", "completed", "call_id", e.id, "name", e.name, "arguments", e.arguments)
		}
		stampNamespace(item, s.info.Namespaces)
		s.outputs[e.outputIndex] = item
		out = append(out, s.ev("response.output_item.done", obj("output_index", num(int64(e.outputIndex)), "item", item)))
	}
	if s.textBuffer != "" {
		calls, clean := s.env.parseDSML(s.textBuffer)
		s.dsmlCalls = append(s.dsmlCalls, calls...)
		if clean != "" {
			s.textParts = append(s.textParts, clean)
			if s.msgIndex >= 0 {
				out = append(out, s.ev("response.output_text.delta", obj("item_id", s.msgID, "output_index", num(int64(s.msgIndex)),
					"content_index", num(0), "delta", clean)))
			}
		}
		s.textBuffer = ""
	}
	fullText := strings.Join(s.textParts, "")
	dsml := s.dsmlCalls
	if len(dsml) == 0 {
		if extra, clean := s.env.parseDSML(fullText); len(extra) > 0 {
			dsml, fullText = extra, clean
		}
	}
	if len(dsml) > 0 && len(s.tools) == 0 {
		for _, dc := range dsml {
			outIdx := len(s.outputs)
			item := obj("id", s.env.id("fc_"), "type", "function_call", "status", "completed",
				"call_id", dc.id, "name", dc.name, "arguments", dc.args)
			stampNamespace(item, s.info.Namespaces)
			s.outputs = append(s.outputs, item)
			added := item.Clone()
			added.Set("status", "in_progress")
			added.Set("arguments", "")
			iid, cid := item.Get("id"), item.Get("call_id")
			out = append(out,
				s.ev("response.output_item.added", obj("output_index", num(int64(outIdx)), "item", added)),
				s.ev("response.function_call_arguments.delta", obj("output_index", num(int64(outIdx)), "item_id", iid, "call_id", cid, "delta", dc.args)),
				s.ev("response.function_call_arguments.done", obj("output_index", num(int64(outIdx)), "item_id", iid, "call_id", cid, "arguments", dc.args)),
				s.ev("response.output_item.done", obj("output_index", num(int64(outIdx)), "item", item)))
		}
	}
	hasOther := false
	for _, o := range s.outputs {
		if o != nil {
			hasOther = true
			break
		}
	}
	if s.msgIndex >= 0 || fullText != "" || !hasOther {
		if s.msgIndex < 0 {
			s.msgIndex = s.addOutput(nil)
			out = append(out,
				s.ev("response.output_item.added", obj("output_index", num(int64(s.msgIndex)),
					"item", obj("id", s.msgID, "type", "message", "status", "in_progress", "role", "assistant", "content", []any{}))),
				s.ev("response.content_part.added", obj("item_id", s.msgID, "output_index", num(int64(s.msgIndex)), "content_index", num(0),
					"part", obj("type", "output_text", "text", "", "annotations", []any{}))))
		}
		out = append(out,
			s.ev("response.output_text.done", obj("item_id", s.msgID, "output_index", num(int64(s.msgIndex)), "content_index", num(0), "text", fullText)),
			s.ev("response.content_part.done", obj("item_id", s.msgID, "output_index", num(int64(s.msgIndex)), "content_index", num(0),
				"part", obj("type", "output_text", "text", fullText, "annotations", []any{}))))
		s.outputs[s.msgIndex] = s.msgItem("completed")
		out = append(out, s.ev("response.output_item.done", obj("output_index", num(int64(s.msgIndex)), "item", s.outputs[s.msgIndex])))
	}
	if s.usage == nil || numOf(s.usage.Get("total_tokens")) == 0 {
		outTxt, rsTxt := strings.Join(s.textParts, ""), strings.Join(s.reasonParts, "")
		if outTxt != "" || rsTxt != "" {
			comp := estimateTokens(outTxt) + estimateTokens(rsTxt)
			prompt := 1 // the original estimates str(None): one token
			s.usage = obj("prompt_tokens", num(int64(prompt)), "completion_tokens", num(int64(comp)),
				"total_tokens", num(int64(prompt+comp)),
				"completion_tokens_details", obj("reasoning_tokens", num(int64(estimateTokens(rsTxt)))),
				"prompt_tokens_details", obj("cached_tokens", num(0)))
			s.estimatedUsage = true
		}
	}
	status := "completed"
	if s.finish == "length" || dropped > 0 {
		status = "incomplete"
	}
	final := s.respObj(status)
	if s.finish == "length" {
		final.Set("incomplete_details", obj("reason", "max_output_tokens"))
	}
	out = append(out, s.ev("response.completed", obj("response", final)))
	return out
}

func sortedKeys(m map[int]*toolEntry) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

// stripDataPrefix mirrors the legacy strip_data_prefix: the payload of an SSE
// data line, "" for blank lines, comments and heartbeats.
func stripDataPrefix(line string) string {
	line = strings.TrimSpace(line)
	if line == "" || line[0] == ':' {
		return ""
	}
	for strings.HasPrefix(line, "data:") {
		line = strings.TrimSpace(line[5:])
	}
	if line == "" || line[0] == ':' {
		return ""
	}
	return line
}

// Line consumes one raw upstream SSE line (comments, blanks, [DONE] and
// non-JSON data are handled here) and returns the frames it produces.
func (s *Stream) Line(line []byte) []Frame {
	data := stripDataPrefix(string(line))
	if data == "" {
		return nil
	}
	if data == "[DONE]" {
		s.sawDone = true
		return nil
	}
	chunk, err := jsonx.UnmarshalObj([]byte(data))
	if err != nil {
		return nil
	}
	return s.Chunk(chunk)
}
