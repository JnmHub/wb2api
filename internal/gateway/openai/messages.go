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
	"os"
	"strconv"
	"strings"
	"time"

	"wb2api/internal/gateway/anthropic"
	"wb2api/internal/jsonx"
	"wb2api/internal/pool"
)

func writeAnthropicError(w http.ResponseWriter, status int, msg string) {
	b, _ := jsonx.Marshal(anthropic.ErrorObj(status, msg, ""))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

func writeAnthropicRateLimited(w http.ResponseWriter, wait int, message, detail string) {
	if wait < 1 {
		wait = 60
	}
	text := message
	if text == "" {
		text = fmt.Sprintf("upstream rate limit reached for this model; retry in %ds", wait)
		if detail != "" {
			text += " - " + truncate(detail, 200)
		}
	}
	b, _ := jsonx.Marshal(anthropic.ErrorObj(429, text, "rate_limit_error"))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Retry-After", fmt.Sprint(wait))
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write(append(b, '\n'))
}

// Messages serves the Anthropic Messages API (/v1/messages and
// /v1/messages/count_tokens) on top of the chat pipeline. It is off by default
// (the gateway ignores cache_control, which can cost extra credits): enable it
// with the enable_anthropic_messages setting or WB_ENABLE_ANTHROPIC_MESSAGES=1.
type Messages struct {
	Chat *Handler
}

func (m *Messages) enabled() bool {
	if m.Chat.Settings.AnthropicMessages() {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WB_ENABLE_ANTHROPIC_MESSAGES"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func (m *Messages) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := m.Chat
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	start := time.Now()
	key, ok := h.authenticate(r)
	if !ok {
		writeAnthropicError(w, http.StatusUnauthorized, "missing or invalid API key")
		return
	}
	// fail closed until the Anthropic-to-Chat bridge has verified prompt-cache
	// parity: reject before forwarding or recording any paid usage
	if !m.enabled() {
		writeAnthropicError(w, http.StatusBadRequest, "Anthropic Messages is disabled by default: this gateway "+
			"translates requests to Chat Completions and ignores cache_control. This can cause excessive credit "+
			"consumption. Enable it in the Settings panel or set WB_ENABLE_ANTHROPIC_MESSAGES=1 to opt in at your own risk.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBody())
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeAnthropicError(w, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		writeAnthropicError(w, http.StatusBadRequest, "cannot read request body")
		return
	}
	payload, err := jsonx.UnmarshalObj(body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "request body must be a JSON object")
		return
	}
	opt := anthropic.Options{}
	if h.Catalog != nil {
		opt.MaxOutputTokens = h.Catalog.MaxOutputTokens
	}
	chat, err := anthropic.ToChat(payload, opt)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.HasSuffix(strings.TrimRight(r.URL.Path, "/"), "/count_tokens") {
		b, _ := jsonx.Marshal(jsonObj("input_tokens", anthropic.EstimateChatTokens(chat)))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(append(b, '\n'))
		return
	}
	model, _ := chat.Get("model").(string)
	wantStream := jsonx.Truthy(chat.Get("stream"))
	sink := func(q *request, acc *pool.Account, cancel context.CancelFunc, idle time.Duration, body io.Reader) {
		if wantStream {
			h.messagesStream(q, acc, cancel, idle, body, model)
		} else {
			h.messagesWhole(q, acc, cancel, idle, body, model)
		}
	}
	h.run(w, r, start, key, chat, runOpts{forceStream: true, stream: wantStream, anthropic: true,
		sessionKey: extractSessionKey(r, chat), sink: sink})
}

func jsonObj(k string, n int) *jsonx.Obj {
	o := jsonx.NewObj()
	o.Set(k, json.Number(strconv.Itoa(n)))
	return o
}

func (h *Handler) messagesWhole(q *request, acc *pool.Account, cancel context.CancelFunc, idle time.Duration, body io.Reader, model string) {
	res := h.collect(q.r, cancel, idle, body, model, true)
	ev := res.usageEvent(acc, model, q.keyID, q.effort(h), q.start)
	if res.err != nil {
		ev.Outcome, ev.Status = "upstream_aborted", http.StatusBadGateway
		h.emit(ev)
		if q.r.Context().Err() == nil {
			writeAnthropicError(q.w, http.StatusBadGateway, "upstream stream error: "+res.err.Error())
		}
		return
	}
	chat := chatFromCollected(res)
	out := anthropic.ChatToMessages(chat, anthropic.Env{}, anthropic.DefaultCached)
	b, err := jsonx.Marshal(out)
	if err != nil {
		writeAnthropicError(q.w, http.StatusInternalServerError, "cannot encode response")
		return
	}
	q.w.Header().Set("Content-Type", "application/json")
	q.w.WriteHeader(http.StatusOK)
	_, _ = q.w.Write(append(b, '\n'))
	h.emit(ev)
}

// chatFromCollected rebuilds a chat.completion object from a folded stream.
func chatFromCollected(res *collected) *jsonx.Obj {
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
	chat.Set("id", res.meta.id)
	chat.Set("model", res.meta.model)
	chat.Set("choices", []any{choice})
	if res.usage != nil {
		if u, err := jsonx.UnmarshalObj(res.usage); err == nil {
			chat.Set("usage", u)
		}
	}
	return chat
}

func (h *Handler) messagesStream(q *request, acc *pool.Account, cancelUp context.CancelFunc, idleD time.Duration, body io.Reader, model string) {
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
	if idleD <= 0 {
		idleD = 5 * time.Minute
	}
	idle := time.AfterFunc(idleD, cancelUp)
	defer idle.Stop()

	s := anthropic.NewStream(model, anthropic.Env{}, anthropic.DefaultCached)
	var (
		br           = bufio.NewReaderSize(body, sseReadBlock)
		acc2         []byte
		firstAt      time.Time
		lastAt       time.Time
		nChunks      int
		streamErr    error
		clientClosed bool
	)
	for !clientClosed {
		if br.Buffered() == 0 && flush() != nil {
			clientClosed = true
			break
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
			for _, f := range s.Line(line) {
				if _, werr := bw.Write(f); werr != nil {
					clientClosed = true
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
	switch {
	case clientClosed:
	case streamErr != nil:
		_, _ = bw.Write(anthropic.ErrorFrame("api_error", streamErr.Error()))
	default:
		for _, f := range s.Finish() {
			_, _ = bw.Write(f)
		}
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
	case s.Failed():
		outcome, status = "failed", http.StatusBadGateway
	}
	ev := UsageEvent{At: q.start, Model: model, Account: acc.UID, KeyID: q.keyID, Stream: true, Outcome: outcome,
		Status: status, Elapsed: time.Since(q.start), Usage: usageJSON(s.Usage()), Chunks: nChunks, Effort: q.effort(h)}
	if !firstAt.IsZero() {
		ev.TTFT, ev.Gen = firstAt.Sub(q.start), lastAt.Sub(firstAt)
	}
	h.emit(ev)
}
