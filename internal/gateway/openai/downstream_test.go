package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/store/settings"
)

// The fake implements the ResponseController API, not net/http internals. It
// records deadlines at the actual Write and Flush boundary and can hold a Write
// until the cancellation callback installs an expired deadline.
type downstreamDeadlineProbe struct {
	mu        sync.Mutex
	header    http.Header
	deadline  time.Time
	sets      []time.Time
	writes    []time.Time
	flushes   []time.Time
	body      bytes.Buffer
	setErr    error
	flushErr  error
	blocked   chan struct{}
	interrupt chan struct{}
	blockOnce sync.Once
	breakOnce sync.Once
}

func (w *downstreamDeadlineProbe) Header() http.Header { return w.header }
func (w *downstreamDeadlineProbe) WriteHeader(int)     {}
func (w *downstreamDeadlineProbe) SetWriteDeadline(d time.Time) error {
	w.mu.Lock()
	// Model HTTP/2's sticky write timeout: once an idle deadline expires,
	// installing another future deadline cannot repair the stream.
	stale := !w.deadline.IsZero() && w.deadline.Before(time.Now()) && d.After(time.Now())
	w.deadline = d
	w.sets = append(w.sets, d)
	err := w.setErr
	if stale && err == nil {
		err = context.DeadlineExceeded
	}
	w.mu.Unlock()
	if !d.IsZero() && !d.After(time.Now()) && w.interrupt != nil {
		w.breakOnce.Do(func() { close(w.interrupt) })
	}
	return err
}
func (w *downstreamDeadlineProbe) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.writes = append(w.writes, w.deadline)
	w.mu.Unlock()
	if w.blocked != nil {
		w.blockOnce.Do(func() { close(w.blocked) })
		<-w.interrupt
		return 0, context.Canceled
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.Write(p)
}
func (w *downstreamDeadlineProbe) FlushError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushes = append(w.flushes, w.deadline)
	return w.flushErr
}
func (w *downstreamDeadlineProbe) currentDeadline() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.deadline
}

type downstreamUnwrapper struct{ http.ResponseWriter }

func (w downstreamUnwrapper) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestDownstreamDeadlineWriteFlushAndIdleLifecycle(t *testing.T) {
	probe := &downstreamDeadlineProbe{header: http.Header{}}
	// Exercise ResponseController's Unwrap path too (middleware must not hide
	// SetWriteDeadline or FlushError if it exposes its underlying writer).
	rw := (&Handler{}).newRelayWriter(downstreamUnwrapper{probe}, httptest.NewRequest("POST", "/", nil))
	defer rw.Close()
	if rw.deadline.timeout != 30*time.Second {
		t.Fatalf("default write timeout=%v", rw.deadline.timeout)
	}
	if !probe.currentDeadline().IsZero() {
		t.Fatal("creating relay armed a deadline before any I/O")
	}
	if _, err := rw.WriteString("data: setup\n\n"); err != nil {
		t.Fatal(err)
	}
	if !probe.currentDeadline().IsZero() {
		t.Fatal("buffering alone armed a deadline")
	}
	var semanticFlushes int
	rw.onSemanticFlush = func() {
		probe.mu.Lock()
		n := len(probe.flushes)
		probe.mu.Unlock()
		if n == 0 {
			t.Error("semantic callback preceded the actual flush")
		}
		semanticFlushes++
	}
	if err := rw.firstOutput([]byte(`{"choices":[{"delta":{"role":"assistant"}}]}`)); err != nil {
		t.Fatal(err)
	}
	if semanticFlushes != 0 {
		t.Fatal("role-only setup consumed semantic fast path")
	}
	start := time.Now()
	if err := rw.firstOutput([]byte(`{"choices":[{"delta":{"content":"x"}}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := rw.firstOutput([]byte(`{"choices":[{"delta":{"content":"y"}}]}`)); err != nil {
		t.Fatal(err)
	}
	if semanticFlushes != 1 {
		t.Fatalf("first semantic flush callbacks=%d", semanticFlushes)
	}
	probe.mu.Lock()
	writes, flushes := append([]time.Time(nil), probe.writes...), append([]time.Time(nil), probe.flushes...)
	probe.mu.Unlock()
	if len(writes) != 1 || len(flushes) != 1 {
		t.Fatalf("actual writes=%d flushes=%d", len(writes), len(flushes))
	}
	for _, d := range append(writes, flushes...) {
		if d.Before(start.Add(29*time.Second)) || d.After(time.Now().Add(31*time.Second)) {
			t.Fatalf("actual I/O not armed with default timeout: %v", d)
		}
	}
	if !probe.currentDeadline().IsZero() {
		t.Fatal("deadline left armed during local idle/pacing")
	}
	if err := rw.Flush(); err != nil {
		t.Fatal(err)
	}
	probe.mu.Lock()
	if len(probe.flushes) != 1 {
		t.Error("clean flush performed unnecessary network I/O")
	}
	probe.mu.Unlock()
}

func TestDownstreamDeadlineCancellationInterruptsBlockedWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := &downstreamDeadlineProbe{header: http.Header{}, blocked: make(chan struct{}), interrupt: make(chan struct{})}
	d := newDeadlineWriter(ctx, probe, time.Hour)
	defer d.Close()
	result := make(chan error, 1)
	go func() { _, err := d.Write([]byte("blocked")); result <- err }()
	select {
	case <-probe.blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("write did not reach blocking writer")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked write error=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation failed to interrupt blocked write")
	}
	select {
	case <-d.done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation deadline callback did not finish")
	}
	if got := probe.currentDeadline(); got.IsZero() || got.After(time.Now()) {
		t.Fatalf("Write cleanup erased cancellation's expired deadline: %v", got)
	}
	if n, err := d.Write([]byte("must not reach writer")); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("write after cancellation: n=%d err=%v", n, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); d.Close() }()
	}
	wg.Wait()
	if !probe.currentDeadline().IsZero() {
		t.Fatal("Close did not clear cancellation deadline")
	}
	probe.mu.Lock()
	clears := 0
	for _, deadline := range probe.sets {
		if deadline.IsZero() {
			clears++
		}
	}
	probe.mu.Unlock()
	if clears != 1 {
		t.Fatalf("concurrent/repeated Close cleared deadline %d times", clears)
	}
}

func TestDownstreamDeadlineErrorsAndUnsupportedBestEffort(t *testing.T) {
	t.Run("deadline failure", func(t *testing.T) {
		want := errors.New("cannot arm write")
		probe := &downstreamDeadlineProbe{header: http.Header{}, setErr: want}
		d := newDeadlineWriter(context.Background(), probe, time.Second)
		defer d.Close()
		if n, err := d.Write([]byte("x")); n != 0 || !errors.Is(err, want) {
			t.Fatalf("arm error not propagated: n=%d err=%v", n, err)
		}
		if len(probe.writes) != 0 {
			t.Fatal("deadline error nevertheless performed write")
		}
	})
	t.Run("flush failure suppresses semantic callback", func(t *testing.T) {
		want := errors.New("flush failed")
		probe := &downstreamDeadlineProbe{header: http.Header{}, flushErr: want}
		rw := (&Handler{}).newRelayWriter(probe, httptest.NewRequest("GET", "/", nil))
		defer rw.Close()
		rw.onSemanticFlush = func() { t.Error("semantic callback reported failed flush") }
		_, _ = rw.WriteString("data: x\n\n")
		if err := rw.firstOutput([]byte(`{"choices":[{"delta":{"content":"x"}}]}`)); !errors.Is(err, want) {
			t.Fatalf("flush error=%v", err)
		}
		if !probe.currentDeadline().IsZero() {
			t.Fatal("failed flush left deadline armed")
		}
	})
	t.Run("unsupported is not a fake bound", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		plain := &downstreamUnsupportedWriter{header: http.Header{}, entered: make(chan struct{}), release: make(chan struct{})}
		defer close(plain.release)
		d := newDeadlineWriter(ctx, plain, time.Millisecond)
		defer d.Close()
		result := make(chan error, 1)
		go func() { _, err := d.Write([]byte("legacy")); result <- err }()
		select {
		case <-plain.entered:
		case <-time.After(3 * time.Second):
			t.Fatal("unsupported writer not reached")
		}
		cancel()
		select {
		case <-d.done:
		case <-time.After(3 * time.Second):
			t.Fatal("cancellation callback hung on unsupported writer")
		}
		select {
		case err := <-result:
			t.Fatalf("unsupported writer claimed cancellation interrupted I/O: %v", err)
		default:
		}
		if n, err := d.Write([]byte("after cancel")); n != 0 || !errors.Is(err, context.Canceled) {
			t.Fatalf("unsupported writer accepted new canceled operation: n=%d err=%v", n, err)
		}
		// The adapter cannot interrupt a writer without deadline support. Its
		// owner must supply cancellation; do not pretend this is a hard bound.
		plain.release <- struct{}{}
		select {
		case <-result:
		case <-time.After(3 * time.Second):
			t.Fatal("manually released unsupported writer did not return")
		}
	})
}

type downstreamUnsupportedWriter struct {
	header  http.Header
	entered chan struct{}
	release chan struct{}
}

func (w *downstreamUnsupportedWriter) Header() http.Header { return w.header }
func (w *downstreamUnsupportedWriter) WriteHeader(int)     {}
func (w *downstreamUnsupportedWriter) Write(p []byte) (int, error) {
	close(w.entered)
	<-w.release
	return len(p), nil
}

type downstreamSlotProbe struct {
	acquired atomic.Int32
	released atomic.Int32
}

func (p *downstreamSlotProbe) Acquire(context.Context, string, int, string) (func(), bool, error) {
	p.acquired.Add(1)
	var once sync.Once
	return func() { once.Do(func() { p.released.Add(1) }) }, true, nil
}

func downstreamSmallSocket(t *testing.T, c net.Conn) {
	t.Helper()
	if tcp, ok := c.(*net.TCPConn); ok {
		if err := tcp.SetWriteBuffer(4096); err != nil {
			t.Errorf("set server send buffer: %v", err)
		}
	}
}

// A raw HTTP/1 client sends a complete request then never reads one response
// byte. Small socket buffers force genuine network backpressure, rather than
// relying on a fake writer or a timing assumption that the client is "slow".
func TestDownstreamHTTP1SlowReaderReleasesAllProtocols(t *testing.T) {
	testDownstreamHTTP1SlowReader(t, true)
}

func TestDownstreamNonstreamHTTP1SlowReaderReleasesAllProtocols(t *testing.T) {
	testDownstreamHTTP1SlowReader(t, false)
}

func testDownstreamHTTP1SlowReader(t *testing.T, stream bool) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	for _, protocol := range []string{"chat", "responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			upDone := make(chan struct{})
			e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(upDone)
				w.Header().Set("Content-Type", "text/event-stream")
				frame := relaySSE(strings.Repeat("x", 64<<10))
				// This is deliberately much larger than the two TCP buffers.
				frames := 512
				if !stream {
					frames = 64 // 4MiB aggregate; still far above the socket buffers
				}
				for i := 0; i < frames; i++ {
					if _, err := io.WriteString(w, frame); err != nil {
						return
					}
					if err := http.NewResponseController(w).Flush(); err != nil {
						return
					}
					select {
					case <-r.Context().Done():
						return
					default:
					}
				}
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}), 1, "")
			e.h.DownstreamWriteTimeout = 75 * time.Millisecond
			e.h.AccountInflight = 1
			slots := &downstreamSlotProbe{}
			e.h.Limiter = slots
			name, cap := "slow-reader-key", 1
			key, err := e.h.Settings.CreateKey(settings.KeyInput{Name: &name, ConcurrencyLimit: &cap})
			if err != nil {
				t.Fatal(err)
			}
			var adapter http.Handler = e.h
			body := `{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"hi"}]}`
			switch protocol {
			case "responses":
				adapter = &Responses{Chat: e.h}
				body = `{"model":"glm-5.3","stream":true,"input":"hi"}`
			case "messages":
				adapter = &Messages{Chat: e.h}
				body = `{"model":"glm-5.3","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
			}
			if !stream {
				body = strings.Replace(body, `"stream":true`, `"stream":false`, 1)
			}
			finished := make(chan struct{})
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				adapter.ServeHTTP(w, r)
				close(finished) // after all account/key release defers
			}))
			srv.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
				downstreamSmallSocket(t, c)
				return ctx
			}
			srv.Start()
			defer srv.Close()
			c, err := net.Dial("tcp", srv.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if tcp, ok := c.(*net.TCPConn); ok {
				if err := tcp.SetReadBuffer(4096); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := fmt.Fprintf(c, "POST / HTTP/1.1\r\nHost: local\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", key.Key, len(body), body); err != nil {
				t.Fatal(err)
			}
			select {
			case <-finished:
			case <-time.After(6 * time.Second):
				c.Close()
				t.Fatal("HTTP/1 relay did not unwind stalled downstream write")
			}
			ev := e.event(t)
			if ev.Stream != stream || ev.Outcome != "client_aborted" || ev.KeyID != key.ID {
				t.Fatalf("slow client must not be recorded as completed: %+v", ev)
			}
			if slots.acquired.Load() != 1 || slots.released.Load() != 1 {
				t.Fatalf("shared slot acquired=%d released=%d", slots.acquired.Load(), slots.released.Load())
			}
			e.h.keyMu.Lock()
			gate := e.h.keyGates[key.ID]
			keyReleased := gate != nil && gate.active == 0
			e.h.keyMu.Unlock()
			if !keyReleased {
				t.Fatal("downstream timeout retained API-key slot")
			}
			for _, acc := range e.h.Pool.All() {
				release, ok := acc.TryAcquire(1)
				if !ok {
					t.Fatal("downstream timeout retained local account slot")
				}
				release()
			}
			select {
			case <-upDone:
			case <-time.After(3 * time.Second):
				t.Fatal("stalled downstream left upstream request open")
			}
		})
	}
}

func TestDownstreamNonstreamBlockedOutputCancellationAllProtocols(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	for _, protocol := range []string{"chat", "responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			var upstreamCalls atomic.Int32
			e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				sseUpstream("paid <&> content")(w, r)
			}), 1, "")
			e.h.DownstreamWriteTimeout = time.Hour // only cancellation can interrupt
			e.h.AccountInflight = 1
			slots := &downstreamSlotProbe{}
			e.h.Limiter = slots
			name, cap := "cancel-output", 1
			key, err := e.h.Settings.CreateKey(settings.KeyInput{Name: &name, ConcurrencyLimit: &cap})
			if err != nil {
				t.Fatal(err)
			}
			var adapter http.Handler = e.h
			body := `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`
			switch protocol {
			case "responses":
				adapter = &Responses{Chat: e.h}
				body = `{"model":"glm-5.3","input":"hi"}`
			case "messages":
				adapter = &Messages{Chat: e.h}
				body = `{"model":"glm-5.3","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer "+key.Key)
			probe := &downstreamDeadlineProbe{header: http.Header{}, blocked: make(chan struct{}), interrupt: make(chan struct{})}
			finished := make(chan struct{})
			go func() { adapter.ServeHTTP(probe, r); close(finished) }()
			select {
			case <-probe.blocked:
			case <-time.After(3 * time.Second):
				t.Fatal("nonstream response did not reach blocked output")
			}
			cancel()
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				t.Fatal("nonstream canceled write did not unwind")
			}
			ev := e.event(t)
			if ev.Outcome != "client_aborted" || ev.Status != http.StatusOK || ev.Stream || len(ev.Usage) == 0 || ev.ClientTTFT != 0 {
				t.Fatalf("canceled paid output accounting: %+v", ev)
			}
			select {
			case extra := <-e.events:
				t.Fatalf("duplicate usage: %+v", extra)
			default:
			}
			if upstreamCalls.Load() != 1 || slots.acquired.Load() != 1 || slots.released.Load() != 1 {
				t.Fatalf("replayed/retained paid request: upstream=%d acquired=%d released=%d", upstreamCalls.Load(), slots.acquired.Load(), slots.released.Load())
			}
			e.h.keyMu.Lock()
			g := e.h.keyGates[key.ID]
			released := g != nil && g.active == 0
			e.h.keyMu.Unlock()
			if !released {
				t.Fatal("canceled JSON output retained key")
			}
			for _, acc := range e.h.Pool.All() {
				release, ok := acc.TryAcquire(1)
				if !ok {
					t.Fatal("canceled JSON output retained account")
				}
				release()
			}
		})
	}
}

func TestDownstreamNonstreamCompleteJSONSuccessAndFlushFailure(t *testing.T) {
	for _, failFlush := range []bool{false, true} {
		t.Run(fmt.Sprint(failFlush), func(t *testing.T) {
			probe := &downstreamDeadlineProbe{header: http.Header{}}
			if failFlush {
				probe.flushErr = errors.New("client stopped during flush")
			}
			var events []UsageEvent
			h := &Handler{OnUsage: func(ev UsageEvent) { events = append(events, ev) }}
			r, stop := startLatency(httptest.NewRequest("POST", "/", nil), time.Now().Add(-time.Second))
			defer stop()
			var canceled bool
			h.completeJSON(probe, r, func() { canceled = true }, []byte(`{"content":"<&>"}`), UsageEvent{At: time.Now(), Outcome: "completed", Status: 200, Usage: json.RawMessage(`{"total_tokens":15}`)})
			if len(events) != 1 {
				t.Fatalf("usage count=%d", len(events))
			}
			ev := events[0]
			if failFlush {
				if ev.Outcome != "client_aborted" || ev.ClientTTFT != 0 || !canceled {
					t.Fatalf("flush failure accounting: %+v canceled=%t", ev, canceled)
				}
			} else if ev.Outcome != "completed" || ev.ClientTTFT <= 0 || ev.CancelLatency != 0 || canceled {
				t.Fatalf("successful output accounting: %+v canceled=%t", ev, canceled)
			}
			if probe.body.String() != "{\"content\":\"<&>\"}\n" {
				t.Fatalf("JSON bytes changed: %q", probe.body.String())
			}
			if !probe.currentDeadline().IsZero() {
				t.Fatal("JSON output left deadline armed")
			}
		})
	}
}

// Use the real pacer with a deadline-aware writer. The timer intentionally
// exceeds the write budget; it is stimulus, not synchronization or a wall-clock
// pass/fail threshold. The protocol output must still reach its final chunk.
func TestDownstreamPacingLongerThanWriteDeadline(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			probe := &downstreamDeadlineProbe{header: http.Header{}}
			h := &Handler{DownstreamWriteTimeout: 10 * time.Millisecond}
			body := strings.NewReader(relaySSE("x") + relaySSE("y") + relaySSE("z") + "data: [DONE]\n\n")
			runRelayProtocol(protocol, h, probe, body, time.Second, func() {}, 20)
			probe.mu.Lock()
			text := probe.body.String()
			deadlines := append(append([]time.Time(nil), probe.writes...), probe.flushes...)
			probe.mu.Unlock()
			if !containsRelayText(text, protocol, "z") {
				t.Fatalf("pacing falsely expired downstream write: %s", text)
			}
			if len(deadlines) == 0 {
				t.Fatal("relay did not perform I/O")
			}
			if !probe.currentDeadline().IsZero() {
				t.Fatal("relay completion retained deadline")
			}
		})
	}
}
