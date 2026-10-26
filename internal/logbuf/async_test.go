package logbuf

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type slowBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	gate    chan struct{}
	started chan struct{}
	once    sync.Once
}

func newSlowBuffer() *slowBuffer {
	return &slowBuffer{gate: make(chan struct{}), started: make(chan struct{})}
}

func (s *slowBuffer) Write(p []byte) (int, error) {
	s.once.Do(func() { close(s.started) })
	<-s.gate
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *slowBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestAsyncWriterDoesNotBlockOnSlowSink(t *testing.T) {
	sink := newSlowBuffer()
	w := NewAsyncWriter(sink, 4)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 2; i++ {
			w.Write([]byte("line\n"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Write blocked on a stalled sink")
	}
	close(sink.gate)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := sink.String(); got != "line\nline\n" {
		t.Fatalf("sink got %q", got)
	}
}

func TestAsyncWriterDropsAndCountsWhenFull(t *testing.T) {
	sink := newSlowBuffer()
	w := NewAsyncWriter(sink, 1)
	w.Write([]byte("x\n"))
	<-sink.started // the queue is now empty and the writer is blocked in the sink
	for i := 0; i < 4; i++ {
		w.Write([]byte("x\n"))
	}
	if w.Dropped() < 3 {
		t.Fatalf("dropped = %d, want at least 3", w.Dropped())
	}
	close(sink.gate)
	_ = w.Close()
	if n := strings.Count(sink.String(), "x"); n != 2 {
		t.Fatalf("sink wrote %d lines, want 2", n)
	}
}

func TestAsyncWriterCloseIsIdempotentAndFlushes(t *testing.T) {
	var buf bytes.Buffer
	w := NewAsyncWriter(&buf, 8)
	io.WriteString(w, "a\n")
	io.WriteString(w, "b\n")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "a\nb\n" {
		t.Fatalf("buf = %q", buf.String())
	}
	// Writes after close are dropped, never panic on a closed channel.
	if _, err := w.Write([]byte("c\n")); err != nil {
		t.Fatal(err)
	}
	if w.Dropped() != 1 {
		t.Fatalf("dropped = %d", w.Dropped())
	}
}
