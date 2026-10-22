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
type relayWriter struct {
	*bufio.Writer
	rc              *http.ResponseController
	dirty           bool
	semantic        bool
	deadline        *deadlineWriter
	onSemanticFlush func()
}

func newRelayWriter(w http.ResponseWriter) *relayWriter {
	return &relayWriter{Writer: bufio.NewWriterSize(w, 32<<10), rc: http.NewResponseController(w)}
}

func (w *relayWriter) Write(p []byte) (int, error) {
	w.dirty = true
	return w.Writer.Write(p)
}

func (w *relayWriter) WriteString(s string) (int, error) {
	w.dirty = true
	return w.Writer.WriteString(s)
}

func (w *relayWriter) Flush() error {
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
	if !w.semantic && semanticFrame(data) {
		w.semantic = true
		if err := w.Flush(); err != nil {
			return err
		}
		if w.onSemanticFlush != nil {
			w.onSemanticFlush()
		}
		return nil
	}
	return nil
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
