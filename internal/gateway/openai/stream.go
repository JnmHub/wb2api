package openai

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"wb2api/internal/gateway/leak"
	"wb2api/internal/pool"
)

const sseReadBlock = 64 << 10

// readLine returns the next line without copying when it fits the reader's
// buffer. The returned slice is only valid until the next call.
func readLine(br *bufio.Reader, acc *[]byte) ([]byte, error) {
	line, err := br.ReadSlice('\n')
	if err != bufio.ErrBufferFull {
		return line, err
	}
	*acc = append((*acc)[:0], line...)
	for err == bufio.ErrBufferFull {
		line, err = br.ReadSlice('\n')
		*acc = append(*acc, line...)
	}
	return *acc, err
}

// sseData extracts the payload of an SSE "data:" line; "" for comments,
// heartbeats and blank lines.
func sseData(line []byte) []byte {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] == ':' {
		return nil
	}
	for bytes.HasPrefix(line, []byte("data:")) {
		line = bytes.TrimSpace(line[5:])
	}
	if len(line) == 0 || line[0] == ':' {
		return nil
	}
	return line
}

// tokenEstimator mirrors the legacy estimate: CJK ideographs count one token
// each, everything else 3.6 characters per token. It is only used when the
// upstream reports no usage, and such numbers are flagged as estimates.
type tokenEstimator struct{ cjk, other int }

func (t *tokenEstimator) add(s string) {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			t.cjk++
		} else {
			t.other++
		}
	}
}

func (t *tokenEstimator) tokens() int {
	if t.cjk+t.other == 0 {
		return 0
	}
	o := int(float64(t.other) / 3.6)
	if o < 1 {
		o = 1
	}
	return t.cjk + o
}

type streamMeta struct {
	id      string
	model   string
	created int64
}

func (m *streamMeta) update(top rawObj) {
	if v, ok := top["id"]; ok {
		var s string
		if json.Unmarshal(v, &s) == nil && s != "" {
			m.id = s
		}
	}
	if v, ok := top["model"]; ok {
		var s string
		if json.Unmarshal(v, &s) == nil && s != "" {
			m.model = s
		}
	}
	if v, ok := top["created"]; ok {
		var n int64
		if json.Unmarshal(v, &n) == nil && n != 0 {
			m.created = n
		}
	}
}

type outChunk struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []outChoice `json:"choices"`
}

type outChoice struct {
	Index        int     `json:"index"`
	Delta        any     `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

func (m *streamMeta) chunk(delta any, finish *string) []byte {
	id := m.id
	if id == "" {
		id = "chatcmpl-wb"
	}
	created := m.created
	if created == 0 {
		created = time.Now().Unix()
	}
	b, _ := marshalNoEscape(outChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: m.model,
		Choices: []outChoice{{Index: 0, Delta: delta, FinishReason: finish}}})
	return b
}

type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func usageTotal(u json.RawMessage) int64 {
	var x struct {
		Total json.Number `json:"total_tokens"`
	}
	if json.Unmarshal(u, &x) != nil {
		return 0
	}
	n, _ := x.Total.Int64()
	return n
}

// pipeStream relays an upstream SSE stream to the client.
func (h *Handler) pipeStream(w http.ResponseWriter, r *http.Request, cancelUp func(), idleD time.Duration, body io.Reader,
	acc *pool.Account, model, keyID, effort string, start time.Time) {

	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream; charset=utf-8")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	// Send the headers now: with held-back text the first body byte can be a long
	// way off, and the client should know the connection is alive.
	_ = rc.Flush()
	bw := bufio.NewWriterSize(w, 32<<10)
	flush := func() error {
		if bw.Buffered() == 0 {
			return nil
		}
		if err := bw.Flush(); err != nil {
			return err
		}
		return rc.Flush()
	}
	writeData := func(b []byte) error {
		if _, err := bw.WriteString("data: "); err != nil {
			return err
		}
		if _, err := bw.Write(b); err != nil {
			return err
		}
		_, err := bw.WriteString("\n\n")
		return err
	}

	if idleD <= 0 {
		idleD = 5 * time.Minute
	}
	idle := time.AfterFunc(idleD, cancelUp)
	defer idle.Stop()

	holdMax := h.MaxHoldTime
	if holdMax <= 0 {
		holdMax = 20 * time.Second
	}
	filter := leak.NewFilter(h.FilterMode)
	var (
		br           = bufio.NewReaderSize(body, sseReadBlock)
		acc2         []byte
		meta         = streamMeta{model: model}
		est          tokenEstimator
		lastUsage    json.RawMessage
		pendingFin   string
		synthesized  bool
		emitted      bool
		nChunks      int
		firstAt      time.Time
		lastAt       time.Time
		heldSince    time.Time
		heldTotal    time.Duration
		outcome      = "completed"
		status       = http.StatusOK
		streamErr    error
		clientClosed bool
	)

	emitActions := func(acts []leak.Action) error {
		for _, a := range acts {
			switch a.Kind {
			case leak.Text:
				if a.Text == "" {
					continue
				}
				emitted = true
				if err := writeData(meta.chunk(map[string]string{"content": a.Text}, nil)); err != nil {
					return err
				}
			case leak.Calls:
				synthesized = true
				for i, c := range a.Calls {
					d := toolCallDelta{Index: i, ID: c.ID, Type: "function"}
					d.Function.Name, d.Function.Arguments = c.Name, c.Arguments
					emitted = true
					if err := writeData(meta.chunk(map[string]any{"tool_calls": []toolCallDelta{d}}, nil)); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	noteHeld := func(now time.Time) {
		if filter.Held() > 0 {
			if heldSince.IsZero() {
				heldSince = now
			}
		} else if !heldSince.IsZero() {
			heldTotal += now.Sub(heldSince)
			heldSince = time.Time{}
		}
	}

loop:
	for {
		if br.Buffered() == 0 {
			if err := flush(); err != nil {
				clientClosed = true
				break
			}
		}
		line, err := readLine(br, &acc2)
		if len(line) > 0 {
			idle.Reset(idleD)
			now := time.Now()
			if data := sseData(line); data != nil && !bytes.Equal(data, []byte("[DONE]")) {
				nChunks++
				if firstAt.IsZero() {
					firstAt = now
				}
				lastAt = now
				c, ok := parseChunk(data)
				if !ok {
					if werr := writeData(data); werr != nil {
						clientClosed = true
						break loop
					}
					emitted = true
				} else {
					meta.update(c.top)
					if u := c.usage(); u != nil && (lastUsage == nil || usageTotal(u) >= usageTotal(lastUsage)) {
						lastUsage = append(json.RawMessage(nil), u...)
					}
					for i := range c.deltas {
						if rs := c.reasoning(i); rs != "" {
							est.add(rs)
						}
						piece := c.content(i)
						if piece != "" {
							est.add(piece)
							acts := filter.Feed(piece)
							if len(acts) == 1 && acts[0].Kind == leak.Text && acts[0].Text == piece && filter.Held() == 0 {
								// Common case: nothing held back, forward the
								// upstream chunk as it is (keeps its id/created).
							} else {
								c.removeContent(i)
								if werr := emitActions(acts); werr != nil {
									clientClosed = true
									break loop
								}
							}
							noteHeld(now)
						}
						if fr := c.holdFinish(i); fr != "" {
							pendingFin = fr
						}
					}
					if c.clean() {
						enc, eerr := c.encode()
						if eerr == nil {
							if werr := writeData(enc); werr != nil {
								clientClosed = true
								break loop
							}
							emitted = true
						}
					}
					if !heldSince.IsZero() && now.Sub(heldSince) > holdMax {
						if werr := emitActions(filter.ReleaseHeld()); werr != nil {
							clientClosed = true
							break loop
						}
						noteHeld(now)
					}
				}
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				streamErr = err
			}
			break
		}
	}

	if !clientClosed {
		now := time.Now()
		if werr := emitActions(filter.Flush()); werr != nil {
			clientClosed = true
		}
		noteHeld(now)
		if !clientClosed && (synthesized || pendingFin != "") {
			reason := pendingFin
			if synthesized {
				reason = "tool_calls"
			}
			if werr := writeData(meta.chunk(map[string]string{}, &reason)); werr != nil {
				clientClosed = true
			}
		}
		if !clientClosed && streamErr == nil && !emitted {
			errObj, _ := json.Marshal(map[string]apiError{"error": {Message: "empty upstream stream", Type: "server_error"}})
			_ = writeData(errObj)
		}
		if !clientClosed {
			_, _ = bw.WriteString("data: [DONE]\n\n")
			_ = flush()
		}
	}

	switch {
	case clientClosed || r.Context().Err() != nil:
		outcome = "client_aborted"
	case streamErr != nil:
		outcome = "upstream_aborted"
		status = http.StatusBadGateway
	case pendingFin == "error":
		outcome = "failed"
		status = http.StatusBadGateway
	}

	ev := UsageEvent{At: start, Model: model, Account: acc.UID, KeyID: keyID, Stream: true,
		Outcome: outcome, Status: status, Elapsed: time.Since(start), HeldFor: heldTotal, Usage: lastUsage, Chunks: nChunks, Effort: effort}
	if !firstAt.IsZero() {
		ev.TTFT = firstAt.Sub(start)
		ev.Gen = lastAt.Sub(firstAt)
	}
	if (lastUsage == nil || usageTotal(lastUsage) == 0) && est.tokens() > 0 {
		n := est.tokens()
		// Estimated numbers are flagged so pricing/limits can ignore them; the
		// prompt side is unknown and left at zero rather than invented.
		ev.Usage = json.RawMessage(`{"prompt_tokens":0,"completion_tokens":` + strconv.Itoa(n) + `,"total_tokens":` + strconv.Itoa(n) + `}`)
		ev.Estimated = true
	}
	h.emit(ev)
}

type aggTool struct {
	index int
	id    string
	name  string
	args  bytes.Buffer
}

// collected is an upstream stream folded into one assistant message.
type collected struct {
	meta      streamMeta
	content   string
	reasoning string
	finish    string
	calls     []map[string]any
	usage     json.RawMessage
	estimated bool
	firstAt   time.Time
	lastAt    time.Time
	nChunks   int
	err       error // upstream read error (nil = clean end of stream)
}

// collect reads the whole upstream stream. With repair, tool calls the model
// printed as text are recovered into structured calls.
func (h *Handler) collect(r *http.Request, cancelUp func(), idleD time.Duration, body io.Reader, model string, repair bool) *collected {
	if idleD <= 0 {
		idleD = 5 * time.Minute
	}
	idle := time.AfterFunc(idleD, cancelUp)
	defer idle.Stop()

	br := bufio.NewReaderSize(body, sseReadBlock)
	var (
		acc2      []byte
		content   bytes.Buffer
		reasoning bytes.Buffer
		tools     = map[int]*aggTool{}
		est       tokenEstimator
		sawDone   bool
		res       = &collected{meta: streamMeta{model: model}, finish: "stop"}
	)
	for {
		line, err := readLine(br, &acc2)
		if len(line) > 0 {
			idle.Reset(idleD)
			if data := sseData(line); data != nil && bytes.Equal(data, []byte("[DONE]")) {
				sawDone = true
			} else if data != nil {
				now := time.Now()
				if res.firstAt.IsZero() {
					res.firstAt = now
				}
				res.lastAt = now
				res.nChunks++
				if c, ok := parseChunk(data); ok {
					res.meta.update(c.top)
					if u := c.usage(); u != nil && (res.usage == nil || usageTotal(u) >= usageTotal(res.usage)) {
						res.usage = append(json.RawMessage(nil), u...)
					}
					for i := range c.deltas {
						if s := c.content(i); s != "" {
							content.WriteString(s)
							est.add(s)
						}
						if s := c.reasoning(i); s != "" {
							reasoning.WriteString(s)
							est.add(s)
						}
						if fr := c.finishReason(i); fr != "" {
							res.finish = fr
						}
						if c.deltas[i] != nil {
							if raw, ok := c.deltas[i]["tool_calls"]; ok {
								var tcs []struct {
									Index    *int   `json:"index"`
									ID       string `json:"id"`
									Function struct {
										Name      string `json:"name"`
										Arguments string `json:"arguments"`
									} `json:"function"`
								}
								if json.Unmarshal(raw, &tcs) == nil {
									for _, tc := range tcs {
										idx := len(tools)
										if tc.Index != nil {
											idx = *tc.Index
										}
										t := tools[idx]
										if t == nil {
											t = &aggTool{index: idx}
											tools[idx] = t
										}
										if tc.ID != "" {
											t.id = tc.ID
										}
										t.name += tc.Function.Name
										t.args.WriteString(tc.Function.Arguments)
									}
								}
							}
						}
					}
				}
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				res.err = err
			}
			break
		}
	}
	if res.err == nil && r.Context().Err() != nil {
		res.err = r.Context().Err()
	}
	res.content, res.reasoning = content.String(), reasoning.String()
	idxs := make([]int, 0, len(tools))
	for i := range tools {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	// A stream cut off by max_tokens or a dropped connection leaves the last call
	// with half-written JSON: drop such calls instead of handing the client
	// unparsable arguments. Placeholders with no function name are dropped too.
	truncatedStream := res.finish == "length" || !sawDone
	for _, i := range idxs {
		t := tools[i]
		if t.name == "" {
			continue
		}
		if truncatedStream && truncatedArguments(t.args.String()) {
			continue
		}
		id := t.id
		if id == "" {
			id = newCallID()
		}
		res.calls = append(res.calls, map[string]any{"id": id, "type": "function",
			"function": map[string]any{"name": t.name, "arguments": t.args.String()}})
	}
	if repair && len(tools) == 0 {
		// a call the model printed instead of emitting arrives as content
		if rc, clean, ok := leak.RepairText(res.content); ok {
			res.content = clean
			for _, c := range rc {
				res.calls = append(res.calls, map[string]any{"id": c.ID, "type": "function",
					"function": map[string]any{"name": c.Name, "arguments": c.Arguments}})
			}
			res.finish = "tool_calls"
		}
	}
	switch {
	case len(res.calls) > 0:
		if res.finish == "stop" || res.finish == "" {
			res.finish = "tool_calls"
		}
	case res.finish == "tool_calls":
		// every call was a placeholder: end normally instead of leaving the
		// client waiting for a call that never comes
		res.finish = "stop"
	}
	if (res.usage == nil || usageTotal(res.usage) == 0) && est.tokens() > 0 {
		n := est.tokens()
		res.usage = json.RawMessage(`{"prompt_tokens":0,"completion_tokens":` + strconv.Itoa(n) + `,"total_tokens":` + strconv.Itoa(n) + `}`)
		res.estimated = true
	}
	return res
}

// usageEvent fills the common fields of the usage record of a collected stream.
func (c *collected) usageEvent(acc *pool.Account, model, keyID, effort string, start time.Time) UsageEvent {
	ev := UsageEvent{At: start, Model: model, Account: acc.UID, KeyID: keyID, Outcome: "completed", Status: http.StatusOK,
		Elapsed: time.Since(start), Usage: c.usage, Estimated: c.estimated, Chunks: c.nChunks, Effort: effort}
	if !c.firstAt.IsZero() {
		ev.TTFT, ev.Gen = c.firstAt.Sub(start), c.lastAt.Sub(c.firstAt)
	}
	return ev
}

// aggregate folds the upstream stream into one chat.completion response.
func (h *Handler) aggregate(w http.ResponseWriter, r *http.Request, cancelUp func(), idleD time.Duration, body io.Reader,
	acc *pool.Account, model, keyID, effort string, start time.Time) {

	res := h.collect(r, cancelUp, idleD, body, model, true)
	ev := res.usageEvent(acc, model, keyID, effort, start)
	if res.err != nil {
		ev.Outcome, ev.Status = "upstream_aborted", http.StatusBadGateway
		h.emit(ev)
		if r.Context().Err() == nil {
			writeError(w, http.StatusBadGateway, "upstream stream aborted: "+res.err.Error(), "server_error")
		}
		return
	}
	msg := map[string]any{"role": "assistant", "content": res.content}
	if res.reasoning != "" {
		msg["reasoning_content"] = res.reasoning
	}
	if len(res.calls) > 0 {
		msg["tool_calls"] = res.calls
	}
	id := res.meta.id
	if id == "" {
		id = "chatcmpl-wb"
	}
	created := res.meta.created
	if created == 0 {
		created = time.Now().Unix()
	}
	resp := map[string]any{
		"id": id, "object": "chat.completion", "created": created, "model": res.meta.model,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": res.finish}},
	}
	if res.usage != nil {
		resp["usage"] = res.usage
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(resp)
	h.emit(ev)
}

// truncatedArguments: a non-empty argument string that is not valid JSON.
func truncatedArguments(raw string) bool {
	t := strings.TrimSpace(raw)
	return t != "" && !json.Valid([]byte(t))
}

func newCallID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "call_" + hex.EncodeToString(b[:])
}
