package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// deadlineWriter arms a fresh deadline only for actual writes. Pacing/idle time
// never consumes it. net/http's ResponseController supports HTTP/1 and HTTP/2;
// adapters which do not expose deadlines retain legacy writes (not a fake bound).
type deadlineWriter struct {
	out         io.Writer
	rc          *http.ResponseController
	timeout     time.Duration
	ctx         context.Context
	mu          sync.Mutex
	canceled    bool
	unsupported bool
	stop        func() bool
	done        chan struct{}
	closeOnce   sync.Once
}

func newDeadlineWriter(ctx context.Context, out http.ResponseWriter, timeout time.Duration) *deadlineWriter {
	d := &deadlineWriter{out: out, rc: http.NewResponseController(out), timeout: timeout, ctx: ctx, done: make(chan struct{})}
	d.stop = context.AfterFunc(ctx, func() {
		noteCancellation(ctx)
		d.mu.Lock()
		d.canceled = true
		if !d.unsupported {
			_ = d.rc.SetWriteDeadline(time.Now())
		}
		d.mu.Unlock()
		close(d.done)
	})
	return d
}

func (d *deadlineWriter) arm() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.canceled || d.ctx.Err() != nil {
		return d.ctx.Err()
	}
	if d.unsupported || d.timeout < 0 {
		return nil
	}
	err := d.rc.SetWriteDeadline(time.Now().Add(d.timeout))
	if errors.Is(err, http.ErrNotSupported) {
		d.unsupported = true
		return nil
	}
	return err
}

func (d *deadlineWriter) disarm() {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Never erase the cancellation deadline while another operation can use it.
	if !d.canceled && d.ctx.Err() == nil && !d.unsupported {
		_ = d.rc.SetWriteDeadline(time.Time{})
	}
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	if err := d.arm(); err != nil {
		noteCancellation(d.ctx)
		return 0, err
	}
	defer d.disarm()
	n, err := d.out.Write(p)
	if err != nil {
		noteCancellation(d.ctx)
	}
	return n, err
}

func (d *deadlineWriter) Close() {
	d.closeOnce.Do(func() {
		if !d.stop() {
			<-d.done
		}
		d.mu.Lock()
		if !d.unsupported {
			_ = d.rc.SetWriteDeadline(time.Time{})
		}
		d.mu.Unlock()
	})
}

// completeJSON sends an already encoded successful nonstream response. Its
// ClientTTFT is the successful flush of the complete JSON document, not the
// upstream first token or proof of receipt by the remote client. Delivery failure
// is terminal: the paid generation must never be replayed or followed by a second
// error envelope after a partial 200 response.
func (h *Handler) completeJSON(w http.ResponseWriter, r *http.Request, cancelUp func(), b []byte, ev UsageEvent) {
	timeout := h.DownstreamWriteTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	d := newDeadlineWriter(r.Context(), w, timeout)
	defer d.Close()
	w.Header().Set("Content-Type", "application/json")
	// net/http buffers WriteHeader; the first actual network write is bounded
	// by d.Write or the explicitly armed Flush below.
	w.WriteHeader(http.StatusOK)
	b = append(b, '\n')
	n, err := d.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = d.arm()
		if err == nil {
			err = d.rc.Flush()
			// Middleware without Flush support retains its legacy Write behavior,
			// but must not claim a measured successful client flush.
			if errors.Is(err, http.ErrNotSupported) {
				err = nil
			} else if err == nil {
				noteClientFlush(r.Context())
			}
			d.disarm()
		}
	}
	if err != nil || r.Context().Err() != nil {
		noteCancellation(r.Context())
		ev.Outcome, ev.Status = "client_aborted", http.StatusOK
		cancelUp()
	}
	ev.Elapsed = time.Since(ev.At)
	h.emitWithLatency(r.Context(), ev)
}

func (h *Handler) newRelayWriter(w http.ResponseWriter, r *http.Request) *relayWriter {
	timeout := h.DownstreamWriteTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	d := newDeadlineWriter(r.Context(), w, timeout)
	out := newRelayWriter(w)
	out.Writer.Reset(d)
	out.deadline = d
	out.onSemanticFlush = func() { noteClientFlush(r.Context()) }
	return out
}
