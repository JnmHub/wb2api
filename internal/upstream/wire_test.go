package upstream

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/upstream/identity"
)

func testTimeouts() Timeouts {
	return Timeouts{Dial: 2 * time.Second, TLS: 2 * time.Second, ResponseHeader: 2 * time.Second, Idle: 5 * time.Second}
}

// rawServer accepts connections and records every raw request it receives
// (headers + body by Content-Length), answering with a fixed 200 response.
type rawServer struct {
	ln       net.Listener
	mu       sync.Mutex
	requests [][]byte
	conns    atomic.Int32
}

func newRawServer(t *testing.T, respond func(c net.Conn, br *bufio.Reader)) *rawServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &rawServer{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.conns.Add(1)
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					raw, err := readRawRequest(br)
					if err != nil {
						return
					}
					s.mu.Lock()
					s.requests = append(s.requests, raw)
					s.mu.Unlock()
					respond(c, br)
				}
			}()
		}
	}()
	return s
}

func readRawRequest(br *bufio.Reader) ([]byte, error) {
	var buf bytes.Buffer
	clen := 0
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		buf.WriteString(line)
		if line == "\r\n" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			clen = atoi(strings.TrimSpace(line[len("content-length:"):]))
		}
	}
	body := make([]byte, clen)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, err
	}
	buf.Write(body)
	return buf.Bytes(), nil
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

func ok200(c net.Conn, _ *bufio.Reader) {
	body := "hello"
	io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 5\r\n\r\n"+body)
}

func (s *rawServer) url() string {
	return "http://" + s.ln.Addr().String() + "/v2/chat/completions?x=1"
}

func TestWireWritesHeadersInGivenOrderAndSpelling(t *testing.T) {
	srv := newRawServer(t, ok200)
	w := NewWire(testTimeouts(), nil)
	defer w.CloseIdle()
	hs := identity.Headers{
		identity.P("Content-Type", "application/json"), identity.P("X-IDE-Type", "WorkBuddy"),
		identity.P("x-stainless-lang", "js"), identity.P("X-Request-ID", "abc"), identity.P("b3", "1-2-1"), identity.P("Authorization", "Bearer t"),
	}
	resp, err := w.Do(context.Background(), Request{Method: "POST", URL: srv.url(), Headers: hs, Body: []byte(`{"a":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "hello" || resp.StatusCode != 200 {
		t.Fatalf("resp %d %q", resp.StatusCode, b)
	}
	srv.mu.Lock()
	raw := string(srv.requests[0])
	srv.mu.Unlock()
	host := srv.ln.Addr().String()
	want := "POST /v2/chat/completions?x=1 HTTP/1.1\r\n" +
		"Accept-Encoding: identity\r\n" +
		"Content-Length: 7\r\n" +
		"Host: " + host + "\r\n" +
		"Content-Type: application/json\r\n" +
		"X-IDE-Type: WorkBuddy\r\n" +
		"x-stainless-lang: js\r\n" +
		"X-Request-ID: abc\r\n" +
		"b3: 1-2-1\r\n" +
		"Authorization: Bearer t\r\n" +
		"Connection: keep-alive\r\n\r\n" +
		`{"a":1}`
	if raw != want {
		t.Fatalf("wire bytes differ\n got:\n%q\nwant:\n%q", raw, want)
	}
}

func TestWireDropsHeaderInjection(t *testing.T) {
	srv := newRawServer(t, ok200)
	w := NewWire(testTimeouts(), nil)
	defer w.CloseIdle()
	resp, err := w.Do(context.Background(), Request{URL: srv.url(), Headers: identity.Headers{
		identity.P("X-Evil", "a\r\nInjected: 1"), identity.P("Bad:Name", "v"), identity.P("X-Good", "ok"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	srv.mu.Lock()
	raw := string(srv.requests[0])
	srv.mu.Unlock()
	if strings.Contains(raw, "Injected") || strings.Contains(raw, "Bad:Name") || !strings.Contains(raw, "X-Good: ok") {
		t.Fatalf("injection not blocked:\n%q", raw)
	}
}

func TestWireReusesConnectionAfterFullRead(t *testing.T) {
	srv := newRawServer(t, ok200)
	w := NewWire(testTimeouts(), nil)
	defer w.CloseIdle()
	for i := 0; i < 5; i++ {
		resp, err := w.Do(context.Background(), Request{URL: srv.url(), Body: []byte("x")})
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if n := srv.conns.Load(); n != 1 {
		t.Fatalf("expected one pooled connection, server saw %d", n)
	}
}

func TestWireDoesNotPoolUnreadOrAbortedBodies(t *testing.T) {
	srv := newRawServer(t, ok200)
	w := NewWire(testTimeouts(), nil)
	defer w.CloseIdle()
	for i := 0; i < 3; i++ {
		resp, err := w.Do(context.Background(), Request{URL: srv.url(), Body: []byte("x")})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close() // closed without reading to EOF
	}
	if n := srv.conns.Load(); n != 3 {
		t.Fatalf("an unread body must not return its connection to the pool; server saw %d conns", n)
	}
}

func TestWireRetriesOnceWhenPooledConnectionDied(t *testing.T) {
	srv := newRawServer(t, ok200)
	w := NewWire(testTimeouts(), nil)
	defer w.CloseIdle()
	warmWire(t, w, srv.url())
	// A locally closed pooled socket deterministically fails before writing
	// any bytes. Peer FIN timing cannot prove that property on all platforms.
	changePooledConn(t, w, func(c net.Conn) net.Conn { c.Close(); return c })
	warmWire(t, w, srv.url())
	if srv.conns.Load() != 2 {
		t.Fatalf("expected one fresh connection after zero-byte stale write, got %d", srv.conns.Load())
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.requests) != 2 {
		t.Fatalf("expected exactly two requests, got %d", len(srv.requests))
	}
}

func TestWireCancelAbortsStreamingBody(t *testing.T) {
	srv := newRawServer(t, func(c net.Conn, _ *bufio.Reader) {
		io.WriteString(c, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n")
		time.Sleep(10 * time.Second) // never finishes
	})
	w := NewWire(testTimeouts(), nil)
	defer w.CloseIdle()
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := w.Do(ctx, Request{URL: srv.url(), Body: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("first chunk: %v %q", err, buf)
	}
	done := make(chan error, 1)
	go func() { _, err := resp.Body.Read(buf); done <- err }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("read should fail after cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not unblock a stalled body read")
	}
	resp.Body.Close()
}

func TestWireHeaderTimeout(t *testing.T) {
	srv := newRawServer(t, func(c net.Conn, _ *bufio.Reader) { time.Sleep(3 * time.Second) })
	to := testTimeouts()
	to.ResponseHeader = 200 * time.Millisecond
	w := NewWire(to, nil)
	defer w.CloseIdle()
	start := time.Now()
	_, err := w.Do(context.Background(), Request{URL: srv.url(), Body: []byte("x")})
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %v, header timeout not enforced", time.Since(start))
	}
}

func TestWireChunkedAndConnectionClose(t *testing.T) {
	srv := newRawServer(t, func(c net.Conn, _ *bufio.Reader) {
		io.WriteString(c, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n3\r\nabc\r\n3\r\ndef\r\n0\r\n\r\n")
		c.Close()
	})
	w := NewWire(testTimeouts(), nil)
	defer w.CloseIdle()
	for i := 0; i < 2; i++ {
		resp, err := w.Do(context.Background(), Request{URL: srv.url(), Body: []byte("x")})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != "abcdef" {
			t.Fatalf("body %q", b)
		}
	}
	if srv.conns.Load() != 2 {
		t.Fatalf("Connection: close must not be pooled, server saw %d conns", srv.conns.Load())
	}
}

func TestWireViaHTTPProxyForPlainHTTPUsesAbsoluteForm(t *testing.T) {
	var gotLine atomic.Value
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLine.Store(r.Method + " " + r.RequestURI)
		io.WriteString(w, "via-proxy")
	}))
	defer proxy.Close()
	pu, _ := url.Parse(proxy.URL)
	w := NewWire(testTimeouts(), pu)
	defer w.CloseIdle()
	resp, err := w.Do(context.Background(), Request{URL: "http://example.invalid/v2/x", Body: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "via-proxy" || gotLine.Load() != "POST http://example.invalid/v2/x" {
		t.Fatalf("body %q line %v", b, gotLine.Load())
	}
}

func TestWireConnectTunnelForHTTPS(t *testing.T) {
	// A proxy that accepts CONNECT and then closes: we only assert the CONNECT
	// request (target + credentials) and a clean failure, not a TLS session.
	var seen atomic.Value
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		var sb strings.Builder
		for {
			l, err := br.ReadString('\n')
			if err != nil {
				return
			}
			sb.WriteString(l)
			if l == "\r\n" {
				break
			}
		}
		seen.Store(sb.String())
		io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
	}()
	pu, _ := url.Parse("http://user:pw@" + ln.Addr().String())
	w := NewWire(testTimeouts(), pu)
	_, err := w.Do(context.Background(), Request{URL: "https://upstream.example:8443/x", Body: []byte("x")})
	if err == nil {
		t.Fatal("handshake against a dead tunnel must fail")
	}
	got, _ := seen.Load().(string)
	if !strings.HasPrefix(got, "CONNECT upstream.example:8443 HTTP/1.1\r\n") ||
		!strings.Contains(got, "Proxy-Authorization: Basic dXNlcjpwdw==") {
		t.Fatalf("bad CONNECT request: %q", got)
	}
}

func TestWireBodyCloseDoesNotDrainUnterminatedResponse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		_, _ = readRawRequest(br)
		_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 100\r\nConnection: close\r\n\r\npartial")
		_, _ = io.Copy(io.Discard, c)
	}()
	w := NewWire(testTimeouts(), nil)
	defer w.CloseIdle()
	resp, err := w.Do(context.Background(), Request{URL: "http://" + ln.Addr().String() + "/error"})
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		_ = resp.Body.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("response body Close blocked on unterminated upstream body")
	}
}

func TestManagerGivesEachAccountItsOwnWire(t *testing.T) {
	m := NewManager(testTimeouts())
	a1, a2 := m.Wire("a", ""), m.Wire("b", "")
	if a1 == a2 || a1 != m.Wire("a", "") {
		t.Fatal("wires must be per account and cached")
	}
	if m.Wire("a", "http://p:1") == a1 {
		t.Fatal("a proxy change must produce a different wire")
	}
}

func TestStaleErrWindowsAbort(t *testing.T) {
	err := errors.New("read tcp 127.0.0.1:1->127.0.0.1:2: wsarecv: An established connection was aborted by the software in your host machine.")
	if !staleErr(err) {
		t.Fatal("a Windows wsarecv abort must classify as possibly stale (not proof of safe replay)")
	}
}
