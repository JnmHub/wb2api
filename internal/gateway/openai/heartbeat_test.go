package openai

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHeartbeatWritesCommentWhenStreamIsQuiet(t *testing.T) {
	w := &relayTestWriter{header: http.Header{}}
	bw := newRelayWriter(w)
	if err := bw.heartbeat(time.Now(), sseHeartbeat); err != nil {
		t.Fatal(err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.body.String(), ": keepalive") {
		t.Fatalf("no keepalive comment: %q", w.body.String())
	}
}

func TestHeartbeatSkipsRecentlyActiveStream(t *testing.T) {
	w := &relayTestWriter{header: http.Header{}}
	bw := newRelayWriter(w)
	if _, err := bw.WriteString(relaySSE("a")); err != nil {
		t.Fatal(err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatal(err)
	}
	before := w.body.String()
	if err := bw.heartbeat(time.Now(), sseHeartbeat); err != nil {
		t.Fatal(err)
	}
	if got := w.body.String(); got != before {
		t.Fatalf("heartbeat wrote into a busy stream: %q", got)
	}
}

func TestHeartbeatFlushesPendingOutputInsteadOfComment(t *testing.T) {
	w := &relayTestWriter{header: http.Header{}}
	bw := newRelayWriter(w)
	if _, err := bw.WriteString(relaySSE("b")); err != nil {
		t.Fatal(err)
	}
	if err := bw.heartbeat(time.Now().Add(time.Hour), sseHeartbeat); err != nil {
		t.Fatal(err)
	}
	out := w.body.String()
	if strings.Contains(out, ": keepalive") {
		t.Fatalf("comment written while output was pending: %q", out)
	}
	if !strings.Contains(out, `"content":"b"`) {
		t.Fatalf("pending output was not flushed: %q", out)
	}
}

type failingWriter struct{ header http.Header }

func (w *failingWriter) Header() http.Header { return w.header }
func (w *failingWriter) WriteHeader(int)     {}
func (w *failingWriter) Write(p []byte) (int, error) {
	return 0, http.ErrHandlerTimeout
}
func (w *failingWriter) Flush() {}

// A failing pace writes means the client is gone; the upstream request must be
// cancelled instead of waiting for the idle timeout.
func TestHeartbeatCancelsUpstreamWhenClientIsGone(t *testing.T) {
	bw := newRelayWriter(&failingWriter{header: http.Header{}})
	var cancelled atomic.Bool
	stop := startHeartbeat(bw, 5*time.Millisecond, func() { cancelled.Store(true) })
	defer stop()
	deadline := time.Now().Add(time.Second)
	for !cancelled.Load() {
		if time.Now().After(deadline) {
			t.Fatal("heartbeat did not cancel after a failed write")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
