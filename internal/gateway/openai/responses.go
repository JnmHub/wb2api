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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"wb2api/internal/gateway/responses"
	"wb2api/internal/jsonx"
	"wb2api/internal/pool"
	"wb2api/internal/upstream"
	"wb2api/internal/upstream/identity"
	"wb2api/internal/webtools"
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
	// Local web_search / web_fetch: swap the server-tool declarations for the
	// gateway's own functions and run the calls ourselves (opt-in).
	if h.Settings != nil && h.Settings.LocalWebTools() {
		if search, fetch := webtools.WantsWeb(info.Tools); search || fetch {
			webtools.InstallDefs(chat, search, fetch)
			info.WebTools = true
			h.logger().Info("local web tools installed", "search", search, "fetch", fetch)
		}
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

// ---------------------------------------------------- local web tools (loop)

type webCall struct{ ID, Name, Args string }

func maxWebRounds() int {
	n := 3
	if v := strings.TrimSpace(os.Getenv("WB_MAX_WEB_ROUNDS")); v != "" {
		if x, err := strconv.Atoi(v); err == nil && x > 0 {
			n = x
		}
	}
	if n > 8 {
		n = 8
	}
	return n
}

func chunkInt(v any) int {
	switch t := v.(type) {
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n)
		}
	case float64:
		return int(t)
	case int:
		return t
	}
	return 0
}

// filterWebChunk removes the gateway's own tool calls from a chat chunk and
// accumulates their arguments. It reports whether the chunk carried one.
func filterWebChunk(chunk *jsonx.Obj, pending map[int]*webCall) bool {
	choices, _ := chunk.Get("choices").([]any)
	internal := false
	for _, raw := range choices {
		ch, ok := raw.(*jsonx.Obj)
		if !ok {
			continue
		}
		delta, ok := ch.Get("delta").(*jsonx.Obj)
		if !ok {
			continue
		}
		tcs, _ := delta.Get("tool_calls").([]any)
		if len(tcs) == 0 {
			continue
		}
		var kept []any
		for _, rawTC := range tcs {
			tc, ok := rawTC.(*jsonx.Obj)
			if !ok {
				kept = append(kept, rawTC)
				continue
			}
			idx := chunkInt(tc.Get("index"))
			fn, _ := tc.Get("function").(*jsonx.Obj)
			name := ""
			if fn != nil {
				name = strings.TrimSpace(jsonx.PyStrIfTruthy(fn.Get("name")))
			}
			if name == "" {
				if c := pending[idx]; c != nil {
					name = c.Name
				}
			}
			if !webtools.WebToolName(name) {
				kept = append(kept, rawTC)
				continue
			}
			internal = true
			c := pending[idx]
			if c == nil {
				c = &webCall{}
				pending[idx] = c
			}
			if id := strings.TrimSpace(jsonx.PyStrIfTruthy(tc.Get("id"))); id != "" {
				c.ID = id
			}
			if fn != nil {
				if n := strings.TrimSpace(jsonx.PyStrIfTruthy(fn.Get("name"))); n != "" {
					c.Name = n
				}
				c.Args += jsonx.PyStrIfTruthy(fn.Get("arguments"))
			}
		}
		if len(kept) == 0 {
			delta.Delete("tool_calls")
		} else {
			delta.Set("tool_calls", kept)
		}
	}
	return internal
}

func orderedCalls(pending map[int]*webCall) []webCall {
	keys := make([]int, 0, len(pending))
	for k := range pending {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	out := make([]webCall, 0, len(keys))
	for _, k := range keys {
		out = append(out, *pending[k])
	}
	return out
}

func appendWebMessages(chat *jsonx.Obj, calls []webCall, results []string) *jsonx.Obj {
	next := chat.Clone()
	msgs, _ := next.Get("messages").([]any)
	var tcs []any
	for _, c := range calls {
		fn := jsonx.NewObj()
		fn.Set("name", c.Name)
		fn.Set("arguments", c.Args)
		tc := jsonx.NewObj()
		tc.Set("id", c.ID)
		tc.Set("type", "function")
		tc.Set("function", fn)
		tcs = append(tcs, tc)
	}
	assistant := jsonx.NewObj()
	assistant.Set("role", "assistant")
	assistant.Set("content", nil)
	assistant.Set("tool_calls", tcs)
	msgs = append(msgs, assistant)
	for i, c := range calls {
		m := jsonx.NewObj()
		m.Set("role", "tool")
		m.Set("tool_call_id", c.ID)
		m.Set("name", c.Name)
		m.Set("content", results[i])
		msgs = append(msgs, m)
	}
	next.Set("messages", msgs)
	return next
}

// disableWebTools removes the gateway's tool definitions and tells the model to
// answer with what it already has.
func disableWebTools(chat *jsonx.Obj) *jsonx.Obj {
	next := chat.Clone()
	if list, ok := next.Get("tools").([]any); ok {
		var kept []any
		for _, raw := range list {
			if o, ok := raw.(*jsonx.Obj); ok {
				name := strings.TrimSpace(jsonx.PyStrIfTruthy(o.Get("name")))
				if fn, ok := o.Get("function").(*jsonx.Obj); ok {
					name = strings.TrimSpace(jsonx.PyStrIfTruthy(fn.Get("name")))
				}
				if webtools.WebToolName(name) {
					continue
				}
			}
			kept = append(kept, raw)
		}
		if len(kept) == 0 {
			next.Delete("tools")
		} else {
			next.Set("tools", kept)
		}
	}
	msgs, _ := next.Get("messages").([]any)
	sys := jsonx.NewObj()
	sys.Set("role", "system")
	sys.Set("content", "The web tools are no longer available. Answer the user now with what you already have. Do not say that you are searching again.")
	next.Set("messages", append(msgs, sys))
	return next
}

// responseRound owns a single upstream body. Each idle timer must cancel its
// current round, not the initial attempt that may already have finished.
// Stop the round's timer before moving on; its callback captures this value.
type responseRound struct {
	io.Reader
	cancel context.CancelFunc
	once   sync.Once
}

func (r *responseRound) close() {
	r.once.Do(func() {
		if closer, ok := r.Reader.(io.Closer); ok {
			_ = closer.Close()
		}
		if r.cancel != nil {
			r.cancel()
		}
	})
}

// openFollowUp sends another chat request for a web-tool round on the same
// account, returning the upstream stream.
func (h *Handler) openFollowUp(q *request, acc *pool.Account, chat *jsonx.Obj) (io.ReadCloser, error) {
	q2 := *q
	q2.payload = chat
	h.reshape(&q2)
	rec := acc.Record()
	product := identity.NormalizeProduct(rec.Product)
	enc, gz := q2.bodyFor(product)
	hdrs := h.chatHeaders(&q2, rec, product, gz)
	cfg := h.Settings.UpstreamConfig()
	ctx, cancel := context.WithCancel(q.r.Context())
	beginAttempt(ctx)
	resp, err := h.Up.Wire(acc.UID, acc.Proxy()).Do(ctx, upstream.Request{
		Method: http.MethodPost, URL: h.chatURL(acc, product), Headers: hdrs, Body: enc.wire(gz),
		HeaderTimeout: cfg.HeaderTimeout,
	})
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b := readUpstreamErrorBody(resp.Body, cancel, 2*time.Second)
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}, nil
}

func (h *Handler) runWebCalls(ctx context.Context, calls []webCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = webtools.Execute(ctx, c.Name, c.Args)
	}
	return out
}

// emptyWebChunk reports a chunk that carried nothing but the stripped internal
// tool call, so it can be dropped instead of forwarded as an empty delta.
func emptyWebChunk(chunk *jsonx.Obj) bool {
	if u, ok := chunk.Get("usage").(*jsonx.Obj); ok && u.Len() > 0 {
		return false
	}
	choices, _ := chunk.Get("choices").([]any)
	if len(choices) == 0 {
		return true
	}
	for _, raw := range choices {
		ch, ok := raw.(*jsonx.Obj)
		if !ok {
			return false
		}
		if jsonx.PyStrIfTruthy(ch.Get("finish_reason")) != "" {
			return false
		}
		if d, ok := ch.Get("delta").(*jsonx.Obj); !ok || d.Len() > 0 {
			return false
		}
	}
	return true
}

func webCallQuery(c webCall) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(c.Args), &m); err == nil {
		for _, k := range []string{"query", "url"} {
			if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return c.Args
}

// internalCalls returns the calls when every one of them is a gateway web tool.
// A mixed batch is left to the client: we cannot fill a normal tool's result.
func internalCalls(calls []map[string]any) ([]webCall, bool) {
	if len(calls) == 0 {
		return nil, false
	}
	out := make([]webCall, 0, len(calls))
	for _, c := range calls {
		fn, _ := c["function"].(map[string]any)
		name := ""
		args := ""
		if fn != nil {
			name = fmt.Sprint(fn["name"])
			args = fmt.Sprint(fn["arguments"])
		}
		if !webtools.WebToolName(name) {
			return nil, false
		}
		id := fmt.Sprint(c["id"])
		if id == "" || id == "<nil>" {
			id = fmt.Sprintf("call_web_%d_%d", time.Now().UnixMilli()%1000000, len(out))
		}
		out = append(out, webCall{ID: id, Name: name, Args: args})
	}
	return out, true
}

// responsesWhole answers a non-streaming request: the upstream stream is
// folded into one chat message and re-shaped as a Responses object.
func (h *Handler) responsesWhole(q *request, acc *pool.Account, cancel context.CancelFunc, idle time.Duration,
	body io.Reader, model string, info responses.Info) {

	chat := q.payload.Clone()
	current := &responseRound{Reader: body, cancel: cancel}
	defer func() { current.close() }()
	rounds := 0
	if info.WebTools {
		rounds = maxWebRounds()
	}
	var res *collected
	for round := 0; ; round++ {
		res = h.collect(q.r, current.close, idle, current.Reader, model, true)
		current.close()
		if res.err != nil {
			ev := res.usageEvent(acc, model, q.keyID, q.effort(h), q.start)
			markCollectedError(&ev, q.r)
			h.emitWithLatency(q.r.Context(), ev)
			if q.r.Context().Err() == nil {
				writeError(q.w, http.StatusBadGateway, "upstream stream aborted", "server_error")
			}
			return
		}
		if info.WebTools && round < rounds {
			if calls, ok := internalCalls(res.calls); ok {
				results := h.runWebCalls(q.r.Context(), calls)
				next := appendWebMessages(chat, calls, results)
				if round+1 >= rounds {
					next = disableWebTools(next)
				}
				nr, ferr := h.openFollowUp(q, acc, next)
				if ferr != nil {
					ev := res.usageEvent(acc, model, q.keyID, q.effort(h), q.start)
					ev.Outcome, ev.Status = "failed", http.StatusBadGateway
					if q.r.Context().Err() != nil {
						ev.Outcome, ev.Status = "client_aborted", http.StatusOK
					}
					h.emitWithLatency(q.r.Context(), ev)
					if q.r.Context().Err() == nil {
						writeError(q.w, http.StatusBadGateway, "web tool follow-up failed", "server_error")
					}
					return
				}
				chat, current = next, &responseRound{Reader: nr}
				continue
			}
		}
		break
	}
	ev := res.usageEvent(acc, model, q.keyID, q.effort(h), q.start)
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
	chatObj := jsonx.NewObj()
	chatObj.Set("choices", []any{choice})
	if res.usage != nil {
		if u, err := jsonx.UnmarshalObj(res.usage); err == nil {
			chatObj.Set("usage", u)
		}
	}
	out := responses.ChatToResponse(chatObj, model, info, responses.Env{})
	b, err := jsonx.MarshalPy(out)
	if err != nil {
		writeError(q.w, http.StatusInternalServerError, "cannot encode response", "server_error")
		return
	}
	h.completeJSON(q.w, q.r, current.close, b, ev)
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
	bw := h.newRelayWriter(w, q.r)
	defer bw.Close()
	flush := bw.Flush
	write := func(frames []responses.Frame) error {
		for _, f := range frames {
			if _, err := bw.Write(f); err != nil {
				return err
			}
			if err := bw.firstOutput(f); err != nil {
				return err
			}
		}
		return nil
	}

	if idleD <= 0 {
		idleD = 5 * time.Minute
	}
	s := responses.NewStream(model, info, responses.Env{})
	pacer := newStreamPacer(q.maxTPS)
	var (
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
	chat := q.payload.Clone()
	rounds := 0
	if info.WebTools {
		rounds = maxWebRounds()
	}
	current := &responseRound{Reader: body, cancel: cancelUp}
	defer func() { current.close() }()
	for round := 0; !clientClosed; round++ {
		// Capture this round: an expired read must never close its follow-up.
		br := bufio.NewReaderSize(&relayReader{body: current.Reader, idle: idleD, cancel: current.close, beforeRead: func() error {
			err := flush()
			if err != nil {
				clientClosed = true
			}
			return err
		}}, sseReadBlock)
		var acc2 []byte
		pending := map[int]*webCall{}
		noteSemantic := upstreamObserver(q.r.Context())
	readRound:
		for !clientClosed {
			line, err := readLine(br, &acc2)
			if len(line) > 0 {
				noteSemantic(line)
				data := sseData(line)
				if upstreamErr, isErr := upstreamSSEError(data); isErr {
					streamErr = upstreamErr
					break readRound
				}
				switch {
				case data == nil:
					if write(s.Line(line)) != nil {
						clientClosed = true
					}
				case bytes.Equal(data, []byte("[DONE]")):
					if write(s.Line(line)) != nil {
						clientClosed = true
					}
				default:
					chunk, perr := jsonx.UnmarshalObj(data)
					if perr != nil {
						if write(s.Line(line)) != nil {
							clientClosed = true
						}
						break
					}
					now := time.Now()
					if pacer != nil {
						if n := chunkOutputTokens(data); n > 0 {
							if perr := pacer.waitBefore(q.r.Context(), n, flush); perr != nil {
								clientClosed = true
								break
							}
						}
					}
					if firstAt.IsZero() {
						firstAt = now
					}
					lastAt = now
					stripped := info.WebTools && filterWebChunk(chunk, pending)
					if !stripped {
						if write(s.Chunk(chunk)) != nil {
							clientClosed = true
						}
						nChunks++
					} else if !emptyWebChunk(chunk) {
						if write(s.Chunk(chunk)) != nil {
							clientClosed = true
						}
						nChunks++
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
		current.close()
		if q.r.Context().Err() != nil {
			clientClosed = true
		}
		if clientClosed || streamErr != nil {
			break
		}
		calls := orderedCalls(pending)
		if !info.WebTools || round >= rounds || len(calls) == 0 {
			break
		}
		results := h.runWebCalls(q.r.Context(), calls)
		for i, c := range calls {
			id := c.ID
			if id == "" {
				id = fmt.Sprintf("ws_%d_%d", time.Now().UnixMilli()%1000000, i)
			}
			if write(s.WebCall(id, c.Name, webCallQuery(c))) != nil {
				clientClosed = true
				break
			}
		}
		if clientClosed {
			break
		}
		next := appendWebMessages(chat, calls, results)
		if round+1 >= rounds {
			next = disableWebTools(next)
		}
		nr, ferr := h.openFollowUp(q, acc, next)
		if ferr != nil {
			streamErr = ferr
			break
		}
		chat = next
		current = &responseRound{Reader: nr}
	}
	if !clientClosed && streamErr == nil {
		if write(s.Finish()) != nil {
			clientClosed = true
		}
	} else if !clientClosed && q.r.Context().Err() == nil {
		// A broken upstream gets an explicit, sanitized error rather than
		// response.completed or a success terminator.
		if write([]responses.Frame{s.Error("upstream stream aborted")}) != nil {
			clientClosed = true
		}
	}
	if !clientClosed && flush() != nil {
		clientClosed = true
	}

	outcome, status := "completed", http.StatusOK
	switch {
	case clientClosed || q.r.Context().Err() != nil:
		outcome = "client_aborted"
	case streamErr != nil:
		outcome, status = "upstream_aborted", http.StatusBadGateway
	}
	ev := UsageEvent{At: q.start, Model: model, Account: acc.UID, KeyID: q.keyID, Stream: true, Outcome: outcome,
		Status: status, Elapsed: time.Since(q.start), Usage: usageJSON(s.Usage()), Estimated: s.Estimated(), Chunks: nChunks, Effort: q.effort(h)}
	if !firstAt.IsZero() {
		ev.TTFT, ev.Gen = firstAt.Sub(q.start), lastAt.Sub(firstAt)
	}
	h.emitWithLatency(q.r.Context(), ev)
}
