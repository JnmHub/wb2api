package openai

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/gateway/responses"
	"wb2api/internal/jsonx"
)

func relaySSE(content string) string {
	return "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + content + "\"}}]}\n\n"
}

type relayTestWriter struct {
	header  http.Header
	body    bytes.Buffer
	flushed string
	onFlush func(string)
}

func (w *relayTestWriter) Header() http.Header         { return w.header }
func (w *relayTestWriter) WriteHeader(int)             {}
func (w *relayTestWriter) Write(p []byte) (int, error) { return w.body.Write(p) }
func (w *relayTestWriter) Flush() {
	w.flushed = w.body.String()
	if w.onFlush != nil {
		w.onFlush(w.flushed)
	}
}

func runRelayProtocol(protocol string, h *Handler, w http.ResponseWriter, body io.Reader, idle time.Duration, cancel func(), rate float64) {
	r := httptest.NewRequest("POST", "/", nil)
	start := time.Now()
	q := &request{w: w, r: r, start: start, payload: jsonx.NewObj(), maxTPS: rate}
	switch protocol {
	case "chat":
		h.pipeStream(w, r, cancel, idle, body, testAccount(), "m", "", "", start, rate)
	case "responses":
		h.responsesStream(q, testAccount(), cancel, idle, body, "m", responses.Info{})
	case "messages":
		h.messagesStream(q, testAccount(), cancel, idle, body, "m")
	}
}

func relayMarker(protocol, text string) string {
	switch protocol {
	case "responses":
		return `"delta":"` + text + `"`
	case "messages":
		return `"text":"` + text + `"`
	default:
		return `"content":"` + text + `"`
	}
}
func containsRelayText(text, protocol, piece string) bool {
	return strings.Contains(strings.ReplaceAll(text, ": ", ":"), relayMarker(protocol, piece))
}

type relaySplitReader struct {
	first, rest []byte
	beforeRest  func()
}

func (r *relaySplitReader) Read(p []byte) (int, error) {
	if len(r.first) > 0 {
		n := copy(p, r.first)
		r.first = r.first[n:]
		return n, nil
	}
	if r.beforeRest != nil {
		f := r.beforeRest
		r.beforeRest = nil
		f()
	}
	if len(r.rest) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.rest)
	r.rest = r.rest[n:]
	return n, nil
}

func TestRelayFlushesBeforePartialLineRead(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			w := &relayTestWriter{header: http.Header{}}
			// x consumes the first-output flush. y must still flush before the
			// next read, despite the partial data line left inside bufio.
			body := &relaySplitReader{first: []byte(relaySSE("x") + relaySSE("y") + "data: {"), rest: []byte("}\n\ndata: [DONE]\n\n")}
			body.beforeRest = func() {
				if !containsRelayText(w.flushed, protocol, "y") {
					t.Errorf("completed y was held before blocking read: %s", w.flushed)
				}
			}
			runRelayProtocol(protocol, &Handler{}, w, body, time.Second, func() {}, 0)
		})
	}
}

func TestRelayFirstSemanticFlushAndPacingDoesNotTriggerIdle(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			w := &relayTestWriter{header: http.Header{}}
			var first time.Duration
			seen := false
			var sawLast, earlyCancel atomic.Bool
			start := time.Now()
			w.onFlush = func(s string) {
				if containsRelayText(s, protocol, "z") {
					sawLast.Store(true)
				}
				if !seen && containsRelayText(s, protocol, "x") {
					seen = true
					first = time.Since(start)
					if containsRelayText(s, protocol, "y") {
						t.Error("first semantic flush was batched behind later output")
					}
				}
			}
			var cancels atomic.Int32
			body := strings.NewReader("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" + relaySSE("x") + relaySSE("y") + relaySSE("z") + "data: [DONE]\n\n")
			runRelayProtocol(protocol, &Handler{}, w, body, 20*time.Millisecond, func() {
				cancels.Add(1)
				if !sawLast.Load() {
					earlyCancel.Store(true)
				}
			}, 20)
			if earlyCancel.Load() {
				t.Fatal("upstream canceled during local pacing before last output")
			}
			if !seen {
				t.Fatal("missing first output")
			}
			wantCancels := int32(0)
			if protocol == "responses" {
				wantCancels = 1 // responses owns and closes every completed round
			}
			if cancels.Load() != wantCancels {
				t.Fatalf("cancel callbacks=%d, want normal cleanup=%d", cancels.Load(), wantCancels)
			}
			if !containsRelayText(w.flushed, protocol, "z") {
				t.Fatal("lost last output")
			}
			t.Logf("first semantic flush=%s; total=%s", first, time.Since(start))
		})
	}
}

type blockingRelayReader struct{ done <-chan struct{} }

func (r blockingRelayReader) Read([]byte) (int, error) { <-r.done; return 0, context.Canceled }

func TestRelayReadIdleCancelsBlockedUpstream(t *testing.T) {
	done := make(chan struct{})
	var cancels atomic.Int32
	r := &relayReader{body: blockingRelayReader{done: done}, idle: 20 * time.Millisecond, beforeRead: func() error { return nil }, cancel: func() { cancels.Add(1); close(done) }}
	_, err := r.Read(make([]byte, 1))
	if err == nil || cancels.Load() != 1 {
		t.Fatalf("err=%v cancels=%d", err, cancels.Load())
	}
	if _, err = r.Read(make([]byte, 1)); !errors.Is(err, errRelayIdle) {
		t.Fatalf("read after expiry: %v", err)
	}
}

// Some upstream bodies return EOF (not context.Canceled) when closed. Idle
// closure must still be an error in every protocol, never a success terminator.
type relayEOFOnClose struct {
	done chan struct{}
	once sync.Once
}

func (r *relayEOFOnClose) Read([]byte) (int, error) { <-r.done; return 0, io.EOF }
func (r *relayEOFOnClose) Close() error             { r.once.Do(func() { close(r.done) }); return nil }

func TestRelayIdleEOFIsNotSuccess(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			body := &relayEOFOnClose{done: make(chan struct{})}
			w := &relayTestWriter{header: http.Header{}}
			runRelayProtocol(protocol, &Handler{}, w, body, 10*time.Millisecond, func() { _ = body.Close() }, 0)
			if !strings.Contains(w.flushed, "upstream stream aborted") {
				t.Fatalf("idle EOF missing error: %s", w.flushed)
			}
			for _, success := range []string{"[DONE]", "response.completed", "message_stop"} {
				if strings.Contains(w.flushed, success) {
					t.Fatalf("idle EOF emitted %s: %s", success, w.flushed)
				}
			}
		})
	}
}

func TestRelayStaleTimeoutRespectsNewReadDeadline(t *testing.T) {
	var cancels atomic.Int32
	r := &relayReader{cancel: func() { cancels.Add(1) }, deadline: time.Now().Add(time.Hour)}
	r.timer = time.AfterFunc(time.Hour, r.timeout)
	defer r.timer.Stop()
	// Simulate an already runnable callback from the previous read.
	r.timeout()
	if r.expired || cancels.Load() != 0 {
		t.Fatal("stale callback expired the new read")
	}
	r.mu.Lock()
	r.deadline = time.Time{}
	r.mu.Unlock()
	r.timeout()
	if r.expired || cancels.Load() != 0 {
		t.Fatal("callback expired an inactive read")
	}
	r.mu.Lock()
	r.deadline = time.Now().Add(-time.Second)
	r.mu.Unlock()
	r.timeout()
	r.timeout()
	if !r.expired || cancels.Load() != 1 {
		t.Fatalf("expired=%v cancels=%d", r.expired, cancels.Load())
	}
}

func TestRelayWriterFlushesExactBufferAndLargeFrames(t *testing.T) {
	for _, size := range []int{32 << 10, 64 << 10} {
		w := &relayTestWriter{header: http.Header{}}
		out := newRelayWriter(w)
		payload := bytes.Repeat([]byte("x"), size)
		if _, err := out.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := out.Flush(); err != nil {
			t.Fatal(err)
		}
		if w.flushed != string(payload) {
			t.Fatalf("size=%d flushed=%d", size, len(w.flushed))
		}
	}
}

func TestRelaySemanticFrames(t *testing.T) {
	for _, data := range []string{
		`{"choices":[{"delta":{"role":"assistant"}}]}`,
		`{"type":"response.output_text.delta","delta":""}`,
		`{"type":"content_block_start","content_block":{"type":"text","text":""}}`,
	} {
		if semanticFrame([]byte(data)) {
			t.Fatalf("setup is not semantic: %s", data)
		}
	}
	for _, data := range []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"t"}]}}]}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`,
		`{"type":"content_block_delta","delta":{"partial_json":"{"}}`,
		`{"type":"content_block_start","content_block":{"type":"tool_use"}}`,
	} {
		if !semanticFrame([]byte(data)) {
			t.Fatalf("missing semantic output: %s", data)
		}
	}
}

func TestRelayFlushFailureDoesNotReadUpstream(t *testing.T) {
	want := errors.New("downstream closed")
	r := &relayReader{beforeRead: func() error { return want }}
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, want) {
		t.Fatal(err)
	}
}
