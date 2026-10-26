package logbuf

import (
	"io"
	"sync"
	"sync/atomic"
)

// AsyncWriter decouples the request path from stderr. slog writes happen on the
// goroutine that logs; if stderr is a pipe whose reader stalls (docker logs,
// journald under load) a synchronous write blocks the request. Lines are queued
// to a bounded channel and written by one background goroutine, and a full
// queue drops the line and counts it instead of slowing the caller down.
type AsyncWriter struct {
	w       io.Writer
	ch      chan []byte
	done    chan struct{}
	mu      sync.Mutex
	closed  bool
	dropped atomic.Int64
}

// NewAsyncWriter returns a writer buffering up to size lines (default 2000).
func NewAsyncWriter(w io.Writer, size int) *AsyncWriter {
	if size <= 0 {
		size = 2000
	}
	a := &AsyncWriter{w: w, ch: make(chan []byte, size), done: make(chan struct{})}
	go a.loop()
	return a
}

func (a *AsyncWriter) loop() {
	defer close(a.done)
	for line := range a.ch {
		if _, err := a.w.Write(line); err != nil {
			return
		}
	}
}

// Write never blocks on the underlying writer.
func (a *AsyncWriter) Write(p []byte) (int, error) {
	buf := append([]byte(nil), p...)
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		a.dropped.Add(1)
		return len(p), nil
	}
	select {
	case a.ch <- buf:
	default:
		a.dropped.Add(1)
	}
	a.mu.Unlock()
	return len(p), nil
}

// Dropped is how many lines were discarded because the queue was full.
func (a *AsyncWriter) Dropped() int64 { return a.dropped.Load() }

// Close flushes queued lines and stops the writer. Later writes are dropped.
func (a *AsyncWriter) Close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	close(a.ch)
	a.mu.Unlock()
	<-a.done
	return nil
}
