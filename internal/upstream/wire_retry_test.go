package upstream

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func warmWire(t *testing.T, w *Wire, target string) {
	t.Helper()
	resp, err := w.Do(context.Background(), Request{URL: target, Body: []byte("warm")})
	if err != nil {
		t.Fatal(err)
	}
	_, rerr := io.Copy(io.Discard, resp.Body)
	cerr := resp.Body.Close()
	if rerr != nil || cerr != nil {
		t.Fatalf("warm body: read=%v close=%v", rerr, cerr)
	}
}

func changePooledConn(t *testing.T, w *Wire, change func(net.Conn) net.Conn) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, list := range w.idle {
		if len(list) == 1 {
			list[0].Conn = change(list[0].Conn)
			return
		}
	}
	t.Fatal("expected exactly one pooled connection")
}

// limitedWriteConn injects a deterministic short/error write while its accepted
// prefix still goes over a real TCP socket. nil err deliberately violates the
// Writer contract to ensure the transport treats short writes conservatively.
type limitedWriteConn struct {
	net.Conn
	limit   int
	err     error
	written int
	calls   int
}

func (c *limitedWriteConn) Write(p []byte) (int, error) {
	c.calls++
	n := 0
	if c.limit > 0 {
		var err error
		n, err = c.Conn.Write(p[:min(c.limit, len(p))])
		if err != nil {
			return n, err
		}
	}
	c.written += n
	return n, c.err
}

func TestWireAcceptedPOSTNotReplayed(t *testing.T) {
	for _, reused := range []bool{false, true} {
		t.Run(fmt.Sprintf("reused=%t", reused), func(t *testing.T) {
			var seen atomic.Int32
			accepted := int32(1)
			if reused {
				accepted++
			}
			srv := newRawServer(t, func(c net.Conn, br *bufio.Reader) {
				if seen.Add(1) == accepted {
					// readRawRequest has already consumed the entire POST.
					c.Close()
					return
				}
				ok200(c, br)
			})
			w := NewWire(testTimeouts(), nil)
			defer w.CloseIdle()
			if reused {
				warmWire(t, w, srv.url())
			}
			resp, err := w.Do(context.Background(), Request{Method: http.MethodPost, URL: srv.url(), Body: []byte("accepted-once")})
			if resp != nil || !IsAmbiguousRequest(err) {
				t.Fatalf("accepted POST must fail ambiguously without replay: resp=%v err=%v", resp, err)
			}
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("EOF lost from error chain: %v", err)
			}
			if seen.Load() != accepted || srv.conns.Load() != 1 {
				t.Fatalf("POST replayed: requests=%d conns=%d", seen.Load(), srv.conns.Load())
			}
			srv.mu.Lock()
			defer srv.mu.Unlock()
			if !strings.HasSuffix(string(srv.requests[len(srv.requests)-1]), "accepted-once") {
				t.Fatal("peer did not receive the complete POST body")
			}
		})
	}
}

func TestWirePartialWriteNotReplayed(t *testing.T) {
	for _, writeErr := range []error{io.ErrUnexpectedEOF, nil} {
		t.Run(fmt.Sprintf("err=%v", writeErr), func(t *testing.T) {
			srv := newRawServer(t, ok200)
			w := NewWire(testTimeouts(), nil)
			defer w.CloseIdle()
			warmWire(t, w, srv.url())
			var limited *limitedWriteConn
			changePooledConn(t, w, func(c net.Conn) net.Conn {
				limited = &limitedWriteConn{Conn: c, limit: 8, err: writeErr}
				return limited
			})
			resp, err := w.Do(context.Background(), Request{URL: srv.url(), Body: []byte("do-not-replay")})
			if resp != nil || !IsAmbiguousRequest(err) || limited.written != 8 || limited.calls != 1 {
				t.Fatalf("partial write replayed or unmarked: resp=%v err=%v written=%d calls=%d", resp, err, limited.written, limited.calls)
			}
			want := writeErr
			if want == nil {
				want = io.ErrShortWrite
			}
			if !errors.Is(err, want) {
				t.Fatalf("underlying error lost: %v", err)
			}
			if srv.conns.Load() != 1 {
				t.Fatalf("partial write opened %d connections", srv.conns.Load())
			}
		})
	}
}

func TestWireZeroByteStaleWriteRetries(t *testing.T) {
	srv := newRawServer(t, ok200)
	w := NewWire(testTimeouts(), nil)
	defer w.CloseIdle()
	warmWire(t, w, srv.url())
	var limited *limitedWriteConn
	changePooledConn(t, w, func(c net.Conn) net.Conn {
		limited = &limitedWriteConn{Conn: c, err: io.EOF}
		return limited
	})
	warmWire(t, w, srv.url())
	if limited.calls != 1 || limited.written != 0 || srv.conns.Load() != 2 {
		t.Fatalf("zero-byte stale retry: calls=%d written=%d conns=%d", limited.calls, limited.written, srv.conns.Load())
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.requests) != 2 {
		t.Fatalf("unexpected accepted requests: %d", len(srv.requests))
	}
}

func TestWireTLSZeroPlaintextWriteIsAmbiguous(t *testing.T) {
	// A TLS implementation can report n=0 after sending part of a record. The
	// pooled mock tests that Do does not mistake that for proof of zero bytes.
	for _, proxyTLS := range []bool{false, true} {
		t.Run(fmt.Sprintf("proxyTLS=%t", proxyTLS), func(t *testing.T) {
			srv := newRawServer(t, ok200)
			w := NewWire(testTimeouts(), nil)
			defer w.CloseIdle()
			warmWire(t, w, srv.url())
			changePooledConn(t, w, func(c net.Conn) net.Conn {
				return &limitedWriteConn{Conn: c, err: io.EOF}
			})
			target := srv.url()
			if proxyTLS {
				// Existing pooled HTTP-over-HTTPS-proxy sockets are encrypted too.
				w.Proxy, _ = url.Parse("https://127.0.0.1:1")
			} else {
				target = strings.Replace(target, "http://", "https://", 1)
				w.mu.Lock()
				for key, list := range w.idle {
					delete(w.idle, key)
					key = strings.Replace(key, "http://", "https://", 1)
					list[0].key = key
					w.idle[key] = list
					break
				}
				w.mu.Unlock()
			}
			_, err := w.Do(context.Background(), Request{URL: target, Body: []byte("x")})
			if !IsAmbiguousRequest(err) || !errors.Is(err, io.EOF) || srv.conns.Load() != 1 {
				t.Fatalf("encrypted zero-plaintext write replayed: err=%v conns=%d", err, srv.conns.Load())
			}
		})
	}
}

func TestWireCancelAfterPOSTIsAmbiguous(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := newRawServer(t, func(c net.Conn, _ *bufio.Reader) { cancel() })
	w := NewWire(testTimeouts(), nil)
	defer w.CloseIdle()
	_, err := w.Do(ctx, Request{URL: srv.url(), Body: []byte("accepted")})
	if !IsAmbiguousRequest(err) || !errors.Is(err, context.Canceled) {
		t.Fatalf("accepted canceled POST: %v", err)
	}
}

func TestWireCanceledBeforePOSTNotAmbiguous(t *testing.T) {
	srv := newRawServer(t, ok200)
	w := NewWire(testTimeouts(), nil)
	defer w.CloseIdle()
	warmWire(t, w, srv.url())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := w.Do(ctx, Request{URL: srv.url()})
	if IsAmbiguousRequest(err) || !errors.Is(err, context.Canceled) || srv.conns.Load() != 1 {
		t.Fatalf("pre-canceled POST: %v conns=%d", err, srv.conns.Load())
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.requests) != 1 {
		t.Fatalf("canceled request was written: %d requests", len(srv.requests))
	}
}

func TestWireBodyFailureIsAmbiguous(t *testing.T) {
	srv := newRawServer(t, func(c net.Conn, _ *bufio.Reader) {
		io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\npartial")
		c.Close()
	})
	w := NewWire(testTimeouts(), nil)
	defer w.CloseIdle()
	resp, err := w.Do(context.Background(), Request{URL: srv.url()})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	if !IsAmbiguousRequest(err) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("body failure marker/error lost: %v", err)
	}
	if IsAmbiguousRequest(fmt.Errorf("caller: %w", io.EOF)) || !IsAmbiguousRequest(fmt.Errorf("caller: %w", err)) {
		t.Fatal("helper must find only real markers through wrapping")
	}
}

func TestWireHeaderTimeoutIsAmbiguous(t *testing.T) {
	seen := make(chan struct{})
	srv := newRawServer(t, func(c net.Conn, _ *bufio.Reader) { close(seen) })
	to := testTimeouts()
	to.ResponseHeader = 30 * time.Millisecond
	w := NewWire(to, nil)
	defer w.CloseIdle()
	_, err := w.Do(context.Background(), Request{URL: srv.url()})
	<-seen
	var ne net.Error
	if !IsAmbiguousRequest(err) || !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("header timeout semantics lost: %v", err)
	}
}
