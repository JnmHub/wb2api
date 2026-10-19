package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"wb2api/internal/gateway/responses"
	"wb2api/internal/jsonx"
	"wb2api/internal/pool"
)

// defaultResponsesModel answers a request that names no model.
const defaultResponsesModel = "deepseek-v4.1-flash"

// Responses serves POST /v1/responses by translating to chat completions, so
// it shares account selection, failover and identity with Handler.
type Responses struct {
	Chat *Handler
}

func (rh *Responses) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := rh.Chat
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	start := time.Now()
	key, ok := h.authenticate(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing or invalid API key", "invalid_request_error")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBody())
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload too large", "invalid_request_error")
			return
		}
		writeError(w, http.StatusBadRequest, "cannot read request body", "invalid_request_error")
		return
	}
	payload, err := jsonx.UnmarshalObj(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "request body must be a JSON object", "invalid_request_error")
		return
	}
	// The gateway keeps no response state, so it cannot replay a prior turn.
	// Ignoring the field would answer a follow-up as a fresh conversation with
	// the context silently missing; say so instead.
	if jsonx.Truthy(payload.Get("previous_response_id")) {
		writeError(w, http.StatusBadRequest, "previous_response_id is not supported: this gateway does not "+
			"store response state. Send the full conversation in 'input' instead, or use a stateless client.",
			"invalid_request_error")
		return
	}

	opt := responses.Options{PreserveContract: preserveContract(),
		Logf: func(f string, a ...any) { h.logger().Info(fmt.Sprintf(f, a...)) }}
	if h.Catalog != nil {
		opt.MaxOutputTokens = h.Catalog.MaxOutputTokens
	}
	chat, info := responses.ToChat(payload, opt)
	model, _ := chat.Get("model").(string)
	if model == "" {
		model = defaultResponsesModel
		chat.Set("model", model)
	}
	wantStream := jsonx.Truthy(payload.Get("stream"))
	sink := func(q *request, acc *pool.Account, cancel context.CancelFunc, idle time.Duration, body io.Reader) {
		if wantStream {
			h.responsesStream(q, acc, cancel, idle, body, model, info)
		} else {
			h.responsesWhole(q, acc, cancel, idle, body, model, info)
		}
	}
	h.run(w, r, start, key, chat, runOpts{forceStream: true, stream: wantStream,
		sessionKey: extractSessionKey(r, payload), sink: sink})
}

func usageJSON(u *jsonx.Obj) []byte {
	if u == nil {
		return nil
	}
	b, err := jsonx.Marshal(u)
	if err != nil {
		return nil
	}
	return b
}

// responsesWhole answers a non-streaming request: the upstream stream is
// folded into one chat message and re-shaped as a Responses object.
func (h *Handler) responsesWhole(q *request, acc *pool.Account, cancel context.CancelFunc, idle time.Duration,
	body io.Reader, model string, info responses.Info) {

	res := h.collect(q.r, cancel, idle, body, model, false)
	ev := res.usageEvent(acc, model, q.keyID, q.start)
	if res.err != nil {
		ev.Outcome, ev.Status = "upstream_aborted", http.StatusBadGateway
		h.emit(ev)
		if q.r.Context().Err() == nil {
			writeError(q.w, http.StatusBadGateway, "upstream stream error: "+res.err.Error(), "server_error")
		}
		return
	}
	msg := jsonx.NewObj()
	msg.Set("role", "assistant")
	msg.Set("content", res.content)
	if res.reasoning != "" {
		msg.Set("reasoning_content", res.reasoning)
	}
	if len(res.calls) > 0 {
		var calls []any
		for _, c := range res.calls {
			fn := c["function"].(map[string]any)
			f := jsonx.NewObj()
			f.Set("name", fn["name"])
			f.Set("arguments", fn["arguments"])
			tc := jsonx.NewObj()
			tc.Set("id", c["id"])
			tc.Set("type", "function")
			tc.Set("function", f)
			calls = append(calls, tc)
		}
		msg.Set("tool_calls", calls)
	}
	choice := jsonx.NewObj()
	choice.Set("index", json.Number("0"))
	choice.Set("message", msg)
	choice.Set("finish_reason", res.finish)
	chat := jsonx.NewObj()
	chat.Set("choices", []any{choice})
	if res.usage != nil {
		if u, err := jsonx.UnmarshalObj(res.usage); err == nil {
			chat.Set("usage", u)
		}
	}
	out := responses.ChatToResponse(chat, model, info, responses.Env{})
	b, err := jsonx.MarshalPy(out)
	if err != nil {
		writeError(q.w, http.StatusInternalServerError, "cannot encode response", "server_error")
		return
	}
	q.w.Header().Set("Content-Type", "application/json")
	q.w.WriteHeader(http.StatusOK)
	_, _ = q.w.Write(append(b, '\n'))
	h.emit(ev)
}

// responsesStream relays the upstream stream as Responses server-sent events.
func (h *Handler) responsesStream(q *request, acc *pool.Account, cancelUp context.CancelFunc, idleD time.Duration,
	body io.Reader, model string, info responses.Info) {

	w := q.w
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream; charset=utf-8")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
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
	write := func(frames []responses.Frame) error {
		for _, f := range frames {
			if _, err := bw.Write(f); err != nil {
				return err
			}
		}
		return nil
	}

	if idleD <= 0 {
		idleD = 5 * time.Minute
	}
	idle := time.AfterFunc(idleD, cancelUp)
	defer idle.Stop()

	s := responses.NewStream(model, info, responses.Env{})
	var (
		br           = bufio.NewReaderSize(body, sseReadBlock)
		acc2         []byte
		firstAt      time.Time
		lastAt       time.Time
		nChunks      int
		streamErr    error
		clientClosed bool
	)
	// created / in_progress go out at once so the client knows it is connected
	if write(s.Start()) != nil || flush() != nil {
		clientClosed = true
	}
	for !clientClosed {
		if br.Buffered() == 0 {
			if flush() != nil {
				clientClosed = true
				break
			}
		}
		line, err := readLine(br, &acc2)
		if len(line) > 0 {
			idle.Reset(idleD)
			if data := sseData(line); data != nil && !bytes.Equal(data, []byte("[DONE]")) {
				now := time.Now()
				if firstAt.IsZero() {
					firstAt = now
				}
				lastAt = now
				nChunks++
			}
			if write(s.Line(line)) != nil {
				clientClosed = true
				break
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				streamErr = err
			}
			break
		}
	}
	if !clientClosed && streamErr == nil {
		if write(s.Finish()) != nil {
			clientClosed = true
		}
	} else if !clientClosed {
		// a broken upstream gets no response.completed: the client must see an
		// incomplete stream, not a fabricated success
		_, _ = bw.WriteString("data: [DONE]\n\n")
	}
	if !clientClosed {
		_ = flush()
	}

	outcome, status := "completed", http.StatusOK
	switch {
	case clientClosed || q.r.Context().Err() != nil:
		outcome = "client_aborted"
	case streamErr != nil:
		outcome, status = "upstream_aborted", http.StatusBadGateway
	}
	ev := UsageEvent{At: q.start, Model: model, Account: acc.UID, KeyID: q.keyID, Stream: true, Outcome: outcome,
		Status: status, Elapsed: time.Since(q.start), Usage: usageJSON(s.Usage()), Estimated: s.Estimated(), Chunks: nChunks}
	if !firstAt.IsZero() {
		ev.TTFT, ev.Gen = firstAt.Sub(q.start), lastAt.Sub(firstAt)
	}
	h.emit(ev)
}
