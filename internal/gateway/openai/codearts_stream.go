package openai

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"

	"wb2api/internal/upstream/codearts"
)

var (
	errCodeArtsEvent     = errors.New("invalid CodeArts stream event")
	errCodeArtsEventSize = errors.New("CodeArts stream event exceeds the size limit")
	codeArtsDone         = []byte("data: [DONE]\n\n")
)

// codeArtsStream dispatches SSE events before passing them to the shared Chat,
// Responses and Messages relays. TCP reads and individual data lines are not
// event boundaries. A canonical event is always a single data line.
type codeArtsStream struct {
	lines      *bufio.Scanner
	data       []byte
	frame      []byte
	pending    bool
	firstLine  bool
	seenSSE    bool
	seenFinish bool
	done       bool
	names      *codearts.ToolNameMap
	tools      map[int]*codeArtsToolName
	observe    func([]byte)
}

type codeArtsToolName struct {
	call rawObj
	fn   rawObj
	name string
	args strings.Builder
}

func (s *codeArtsStream) restoreToolNames(delta rawObj, finish bool) error {
	if s.names.Empty() {
		return nil
	}
	var calls []rawObj
	if raw := delta["tool_calls"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &calls); err != nil {
			return errCodeArtsEvent
		}
	}
	if len(calls) == 0 && (!finish || len(s.tools) == 0) {
		return nil
	}
	ready := calls[:0]
	for i, call := range calls {
		if call == nil {
			return errCodeArtsEvent
		}
		index := i
		_ = json.Unmarshal(call["index"], &index)
		var fn rawObj
		_ = json.Unmarshal(call["function"], &fn)
		name := rawString(fn["name"])
		pending := s.tools[index]
		if pending == nil && !s.names.HasSentPrefix(name) {
			if original := s.names.RestoreName(name); original != name {
				fn["name"], _ = json.Marshal(original)
				call["function"], _ = marshalNoEscape(fn)
			}
			ready = append(ready, call)
			continue
		}
		if pending == nil {
			pending = &codeArtsToolName{call: rawObj{}, fn: rawObj{}}
			if s.tools == nil {
				s.tools = make(map[int]*codeArtsToolName)
			}
			s.tools[index] = pending
		}
		for k, v := range call {
			pending.call[k] = v
		}
		for k, v := range fn {
			pending.fn[k] = v
		}
		pending.name += name
		arguments := rawString(fn["arguments"])
		if pending.args.Len()+len(arguments) > maxFirstOutputPrelude {
			return errCodeArtsEventSize
		}
		pending.args.WriteString(arguments)
		if finish || !s.names.HasSentPrefix(pending.name) {
			ready = append(ready, s.completeToolName(index, pending))
		}
	}
	if finish {
		var indices []int
		for i := range s.tools {
			indices = append(indices, i)
		}
		sort.Ints(indices)
		for _, i := range indices {
			ready = append(ready, s.completeToolName(i, s.tools[i]))
		}
	}
	if len(ready) == 0 {
		delete(delta, "tool_calls")
	} else {
		delta["tool_calls"], _ = marshalNoEscape(ready)
	}
	return nil
}

func (s *codeArtsStream) completeToolName(index int, name *codeArtsToolName) rawObj {
	delete(s.tools, index)
	name.fn["name"], _ = json.Marshal(s.names.RestoreName(name.name))
	name.fn["arguments"], _ = json.Marshal(name.args.String())
	name.call["index"], _ = json.Marshal(index)
	name.call["function"], _ = marshalNoEscape(name.fn)
	return name.call
}

func rawString(value json.RawMessage) string {
	var s string
	_ = json.Unmarshal(value, &s)
	return s
}

type codeArtsCompressedBody struct {
	base io.Reader
	gzip *gzip.Reader
}

func (b *codeArtsCompressedBody) Read(p []byte) (int, error) {
	if b.gzip == nil {
		var err error
		b.gzip, err = gzip.NewReader(b.base)
		if err != nil {
			return 0, err
		}
	}
	return b.gzip.Read(p)
}

type failedCodeArtsBody struct{ err error }

func (b failedCodeArtsBody) Read([]byte) (int, error) { return 0, b.err }

func codeArtsReplyBody(body io.Reader, contentEncoding string) io.Reader {
	switch strings.ToLower(strings.TrimSpace(contentEncoding)) {
	case "", "identity":
		return body
	case "gzip":
		return &codeArtsCompressedBody{base: body}
	default:
		return failedCodeArtsBody{errors.New("unsupported CodeArts response compression")}
	}
}

func newCodeArtsStream(body io.Reader) *codeArtsStream {
	lines := bufio.NewScanner(body)
	lines.Buffer(make([]byte, sseReadBlock), maxFirstOutputPrelude)
	lines.Split(splitCodeArtsLine)
	return &codeArtsStream{lines: lines, firstLine: true}
}

func splitCodeArtsLine(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		end := i + 1
		if data[i] == '\r' {
			if end == len(data) && !atEOF {
				return 0, nil, nil
			}
			if end < len(data) && data[end] == '\n' {
				end++
			}
		}
		return end, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func (s *codeArtsStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(s.frame) == 0 {
		if s.done {
			return 0, io.EOF
		}
		if !s.lines.Scan() {
			if err := s.lines.Err(); err != nil {
				return 0, err
			}
			// A complete finish frame can itself terminate a CodeArts reply.
			// EOF in the middle of an event or during output is an interruption.
			if s.pending || !s.seenFinish {
				return 0, io.ErrUnexpectedEOF
			}
			if err := s.complete(); err != nil {
				return 0, err
			}
			break
		}
		line := s.lines.Bytes()
		if s.firstLine {
			line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
			s.firstLine = false
		}
		if !s.seenSSE && len(bytes.TrimSpace(line)) > 0 && (s.pending || bytes.HasPrefix(bytes.TrimSpace(line), []byte("{"))) {
			// A gateway can respond HTTP 200 with a JSON fault instead of SSE.
			if err := s.appendData(line); err != nil {
				return 0, err
			}
			if !json.Valid(s.data) {
				continue
			}
			if !utf8.Valid(s.data) {
				return 0, errCodeArtsEvent
			}
			if err := codearts.ParseChatError(s.data); err != nil {
				if s.observe != nil {
					s.observe(s.data)
				}
				return 0, err
			}
			// Some routes ignore stream=true and return a whole chat completion.
			// Feed it through the same normalization before ending the stream.
			if err := s.dispatch(); err != nil {
				return 0, err
			}
			if len(s.frame) == 0 {
				return 0, errCodeArtsEvent
			}
			if err := s.complete(); err != nil {
				return 0, err
			}
			continue
		}
		if len(line) == 0 {
			if s.pending {
				if err := s.dispatch(); err != nil {
					return 0, err
				}
			}
			continue
		}
		s.seenSSE = true
		field, value, _ := bytes.Cut(line, []byte(":"))
		if bytes.Equal(field, []byte("data")) {
			if err := s.appendData(bytes.TrimPrefix(value, []byte(" "))); err != nil {
				return 0, err
			}
		} else if len(field) == 0 && !s.pending {
			// Preserve heartbeat activity without feeding its text to JSON relays.
			s.frame = []byte(": heartbeat\n\n")
		}
	}
	n := copy(p, s.frame)
	s.frame = s.frame[n:]
	return n, nil
}

func (s *codeArtsStream) appendData(value []byte) error {
	if len(s.data)+len(value)+1 > maxFirstOutputPrelude {
		return errCodeArtsEventSize
	}
	if s.pending {
		s.data = append(s.data, '\n')
	}
	s.data = append(s.data, value...)
	s.pending = true
	return nil
}

func (s *codeArtsStream) dispatch() error {
	data := bytes.TrimSpace(s.data)
	s.data, s.pending = s.data[:0], false
	if len(data) == 0 {
		return nil
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		return s.complete()
	}
	if !utf8.Valid(data) {
		return errCodeArtsEvent
	}
	c, ok := parseChunk(data)
	if !ok {
		return errCodeArtsEvent
	}
	if fault := codearts.ParseChatError(data); fault != nil {
		if s.observe != nil {
			s.observe(data)
		}
		return fault
	}
	if c.choices == nil && c.usage() == nil {
		return nil
	}
	if len(c.choices) > 1 {
		return errCodeArtsEvent
	}
	for i, choice := range c.choices {
		if choice == nil {
			return errCodeArtsEvent
		}
		finish := c.finishReason(i)
		choice["index"] = json.RawMessage("0")
		if finish == "" {
			choice["finish_reason"] = json.RawMessage("null")
		}
		if finish == "error" {
			return &codearts.ChatError{Status: http.StatusBadGateway}
		}
		s.seenFinish = s.seenFinish || finish != ""
		if c.deltas[i] == nil {
			delta := rawObj{}
			_ = json.Unmarshal(choice["message"], &delta)
			if delta == nil {
				delta = rawObj{}
			}
			c.deltas[i] = delta
		}
		delete(choice, "message")
		if d := c.deltas[i]; d != nil {
			if isFalsyRaw(d["reasoning_content"]) && !isFalsyRaw(d["reasoning"]) {
				d["reasoning_content"] = d["reasoning"]
			}
			delete(d, "reasoning")
			if err := s.restoreToolNames(d, finish != ""); err != nil {
				return err
			}
		}
	}
	enc, err := c.encode()
	if err != nil {
		return errCodeArtsEvent
	}
	s.writeFrame(enc)
	return nil
}

func (s *codeArtsStream) writeFrame(enc []byte) {
	if s.observe != nil {
		s.observe(enc)
	}
	s.frame = append(append(s.frame, "data: "...), enc...)
	s.frame = append(s.frame, '\n', '\n')
}

func (s *codeArtsStream) complete() error {
	if len(s.tools) > 0 {
		delta := rawObj{}
		if err := s.restoreToolNames(delta, true); err != nil {
			return err
		}
		enc, err := marshalNoEscape(map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": delta, "finish_reason": nil,
		}}})
		if err != nil {
			return err
		}
		s.writeFrame(enc)
	}
	if s.observe != nil {
		s.observe([]byte("[DONE]"))
	}
	s.done, s.frame = true, append(s.frame, codeArtsDone...)
	return nil
}
