package openai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// relayReader flushes completed output before an actual upstream Read, even
// when bufio still holds a partial line. Idle measures upstream I/O only: local
// pacing, conversion and a blocked downstream write are not upstream idleness.
// It is used by a single relay goroutine; only the timeout callback is concurrent.
type relayReader struct {
	body       io.Reader
	beforeRead func() error
	cancel     func()
	idle       time.Duration
	mu         sync.Mutex
	timer      *time.Timer
	deadline   time.Time
	expired    bool
}

var errRelayIdle = errors.New("upstream read idle timeout")

func (r *relayReader) timeout() {
	r.mu.Lock()
	if r.expired || r.deadline.IsZero() {
		r.mu.Unlock()
		return
	}
	// An old callback can already be runnable when Stop is called. Never let
	// it expire a later Read before that Read's own deadline.
	if left := time.Until(r.deadline); left > 0 {
		r.timer.Reset(left)
		r.mu.Unlock()
		return
	}
	r.expired = true
	r.mu.Unlock()
	r.cancel()
}

func (r *relayReader) Read(p []byte) (int, error) {
	if err := r.beforeRead(); err != nil {
		return 0, err
	}
	r.mu.Lock()
	if r.expired {
		r.mu.Unlock()
		return 0, errRelayIdle
	}
	r.deadline = time.Now().Add(r.idle)
	if r.timer == nil {
		r.timer = time.AfterFunc(r.idle, r.timeout)
	} else {
		r.timer.Reset(r.idle)
	}
	r.mu.Unlock()
	n, err := r.body.Read(p)
	r.mu.Lock()
	r.deadline = time.Time{}
	r.timer.Stop()
	expired := r.expired
	r.mu.Unlock()
	if expired {
		// A Close caused by idle can look like EOF to some bodies. It is not a
		// clean end of generation and must not produce a success terminator.
		err = errRelayIdle
	}
	return n, err
}

// relayWriter retains batching after the first semantic frame, but flushes the
// first actual output immediately. dirty also covers writes which filled the
// bufio buffer exactly (Buffered()==0 does not mean the HTTP writer is flushed).
// It is written by the relay goroutine and by the heartbeat goroutine, so the
// buffer, dirty and deadline state are guarded by mu.
type relayWriter struct {
	*bufio.Writer
	rc              *http.ResponseController
	mu              sync.Mutex
	dirty           bool
	semantic        bool
	lastFlush       time.Time
	deadline        *deadlineWriter
	onSemanticFlush func()
}

func newRelayWriter(w http.ResponseWriter) *relayWriter {
	return &relayWriter{Writer: bufio.NewWriterSize(w, 32<<10), rc: http.NewResponseController(w)}
}

func (w *relayWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dirty = true
	return w.Writer.Write(p)
}

func (w *relayWriter) WriteString(s string) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dirty = true
	return w.Writer.WriteString(s)
}

func (w *relayWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushLocked()
}

func (w *relayWriter) flushLocked() error {
	if !w.dirty {
		return nil
	}
	if err := w.Writer.Flush(); err != nil {
		return err
	}
	if w.deadline != nil {
		if err := w.deadline.arm(); err != nil {
			return err
		}
		defer w.deadline.disarm()
	}
	if err := w.rc.Flush(); err != nil {
		if w.deadline != nil {
			noteCancellation(w.deadline.ctx)
		}
		return err
	}
	w.dirty = false
	w.lastFlush = time.Now()
	return nil
}

func (w *relayWriter) Close() {
	if w.deadline != nil {
		w.deadline.Close()
	}
}

// firstOutput is only inspected until the first meaningful frame is emitted.
// Setup/role/empty delta frames must not consume the first-output fast path.
func (w *relayWriter) firstOutput(data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.semantic && semanticFrame(data) {
		w.semantic = true
		if err := w.flushLocked(); err != nil {
			return err
		}
		if w.onSemanticFlush != nil {
			w.onSemanticFlush()
		}
		return nil
	}
	return nil
}

// sseHeartbeat is how long an SSE stream may stay silent before a comment frame
// is written. Proxies and clients drop connections that see no bytes for ~30 s,
// and a model may think for longer than that before its first token.
const sseHeartbeat = 15 * time.Second

// heartbeat writes an SSE comment when nothing went out recently. A pending real
// write is just flushed instead. It runs on its own goroutine.
func (w *relayWriter) heartbeat(now time.Time, every time.Duration) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dirty {
		return w.flushLocked()
	}
	if !w.lastFlush.IsZero() && now.Sub(w.lastFlush) < every*3/4 {
		return nil
	}
	w.dirty = true
	if _, err := w.Writer.WriteString(": keepalive\n\n"); err != nil {
		return err
	}
	return w.flushLocked()
}

// startHeartbeat keeps an SSE stream alive while the upstream is quiet. A
// failing write means the client is gone: the upstream request is cancelled so
// the blocked read returns instead of waiting for the idle timeout.
func startHeartbeat(bw *relayWriter, every time.Duration, cancel func()) func() {
	if every <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-t.C:
				if err := bw.heartbeat(now, every); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

func semanticFrame(data []byte) bool {
	if !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			if bytes.HasPrefix(line, []byte("data:")) {
				return semanticFrame(bytes.TrimSpace(line[5:]))
			}
		}
		return false
	}
	var obj map[string]any
	if json.Unmarshal(data, &obj) != nil {
		return false
	}
	if choices, ok := obj["choices"].([]any); ok {
		for _, raw := range choices {
			choice, _ := raw.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if semanticDelta(delta) {
				return true
			}
		}
	}
	switch str(obj["type"]) {
	case "response.output_text.delta", "response.reasoning_summary_text.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		return str(obj["delta"]) != ""
	case "response.output_item.added":
		item, _ := obj["item"].(map[string]any)
		return str(item["type"]) == "function_call" || str(item["type"]) == "custom_tool_call"
	case "content_block_delta":
		delta, _ := obj["delta"].(map[string]any)
		return str(delta["text"]) != "" || str(delta["thinking"]) != "" || str(delta["partial_json"]) != ""
	case "content_block_start":
		block, _ := obj["content_block"].(map[string]any)
		return str(block["type"]) == "tool_use" || str(block["text"]) != "" || str(block["thinking"]) != ""
	}
	return false
}
