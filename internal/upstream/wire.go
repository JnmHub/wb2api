package upstream

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"wb2api/internal/upstream/identity"
)

// Request is one outbound call with an explicit, ordered header list. Names are
// written exactly as given.
type Request struct {
	Method  string
	URL     string
	Headers identity.Headers
	Body    []byte
	// HeaderTimeout overrides the wire's response-header timeout (0 = default).
	HeaderTimeout time.Duration
}

// Wire is a minimal HTTP/1.1 client that owns the exact bytes put on the
// socket (header order and spelling) and keeps its own idle-connection pool.
// One Wire serves exactly one (account, proxy) pair, so connections are never
// shared between accounts. net/http cannot be used for this: it sorts headers
// alphabetically, canonicalises their names and adds its own.
type Wire struct {
	T     Timeouts
	Proxy *url.URL

	mu   sync.Mutex
	idle map[string][]*wireConn
}

type wireConn struct {
	net.Conn
	br        *bufio.Reader
	key       string
	idleSince time.Time
}

// NewWire creates a Wire; proxy may be nil (direct).
func NewWire(t Timeouts, proxy *url.URL) *Wire {
	return &Wire{T: t, Proxy: proxy, idle: map[string][]*wireConn{}}
}

const maxIdlePerHost = 4
const idleTTL = 90 * time.Second

var (
	tlsOnce sync.Once
	tlsRoot *x509.CertPool
)

// rootPool returns the system roots plus the optional WB_CA_BUNDLE /
// SSL_CERT_FILE PEM (for TLS-decrypting proxies). Verification stays on.
func rootPool() *x509.CertPool {
	tlsOnce.Do(func() {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		for _, env := range []string{"WB_CA_BUNDLE", "SSL_CERT_FILE"} {
			if p := strings.TrimSpace(os.Getenv(env)); p != "" {
				if pem, err := os.ReadFile(p); err == nil {
					pool.AppendCertsFromPEM(pem)
					break
				}
			}
		}
		tlsRoot = pool
	})
	return tlsRoot
}

func splitTarget(raw string) (scheme, host, port, hostHeader, selector string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return
	}
	scheme = strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" || u.Hostname() == "" {
		err = fmt.Errorf("unsupported upstream URL %q", raw)
		return
	}
	host = u.Hostname()
	port = u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[scheme]
	}
	hostHeader = u.Host // netloc with an explicit port when the URL had one
	selector = u.EscapedPath()
	if selector == "" {
		selector = "/"
	}
	if u.RawQuery != "" {
		selector += "?" + u.RawQuery
	}
	return
}

func (w *Wire) takeIdle(key string) *wireConn {
	w.mu.Lock()
	defer w.mu.Unlock()
	list := w.idle[key]
	for len(list) > 0 {
		c := list[len(list)-1]
		list = list[:len(list)-1]
		if time.Since(c.idleSince) > idleTTL {
			c.Close()
			continue
		}
		w.idle[key] = list
		return c
	}
	w.idle[key] = list
	return nil
}

func (w *Wire) putIdle(c *wireConn) {
	c.idleSince = time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.idle[c.key]) >= maxIdlePerHost {
		c.Close()
		return
	}
	w.idle[c.key] = append(w.idle[c.key], c)
}

// CloseIdle closes every pooled connection.
func (w *Wire) CloseIdle() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for k, list := range w.idle {
		for _, c := range list {
			c.Close()
		}
		delete(w.idle, k)
	}
}

func (w *Wire) dial(ctx context.Context, scheme, host, port string) (net.Conn, error) {
	d := net.Dialer{Timeout: w.T.Dial, KeepAlive: 30 * time.Second}
	target := net.JoinHostPort(host, port)
	if w.Proxy == nil {
		raw, err := d.DialContext(ctx, "tcp", target)
		if err != nil {
			return nil, err
		}
		return w.maybeTLS(ctx, raw, scheme, host)
	}
	pport := w.Proxy.Port()
	if pport == "" {
		pport = map[string]string{"http": "80", "https": "443"}[w.Proxy.Scheme]
	}
	raw, err := d.DialContext(ctx, "tcp", net.JoinHostPort(w.Proxy.Hostname(), pport))
	if err != nil {
		return nil, err
	}
	if w.Proxy.Scheme == "https" {
		if raw, err = w.maybeTLS(ctx, raw, "https", w.Proxy.Hostname()); err != nil {
			return nil, err
		}
	}
	if scheme == "http" {
		return raw, nil // plain HTTP through a proxy uses absolute-form requests
	}
	// CONNECT tunnel for https targets.
	_ = raw.SetDeadline(time.Now().Add(w.T.Dial + w.T.TLS))
	var req strings.Builder
	fmt.Fprintf(&req, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if u := w.Proxy.User; u != nil {
		pw, _ := u.Password()
		req.WriteString("Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(u.Username()+":"+pw)) + "\r\n")
	}
	req.WriteString("\r\n")
	if _, err := io.WriteString(raw, req.String()); err != nil {
		raw.Close()
		return nil, err
	}
	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("proxy CONNECT: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		raw.Close()
		return nil, fmt.Errorf("proxy CONNECT refused: %s", resp.Status)
	}
	_ = raw.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		raw.Close()
		return nil, errors.New("proxy sent unexpected data after CONNECT")
	}
	return w.maybeTLS(ctx, raw, scheme, host)
}

func (w *Wire) maybeTLS(ctx context.Context, raw net.Conn, scheme, host string) (net.Conn, error) {
	if scheme != "https" {
		return raw, nil
	}
	cfg := &tls.Config{ServerName: host, NextProtos: []string{"http/1.1"}, RootCAs: rootPool(), MinVersion: tls.VersionTLS12}
	tc := tls.Client(raw, cfg)
	hctx, cancel := context.WithTimeout(ctx, w.T.TLS)
	defer cancel()
	if err := tc.HandshakeContext(hctx); err != nil {
		raw.Close()
		return nil, err
	}
	return tc, nil
}

// staleErr reports an error that can only mean "the pooled connection was
// already dead", so retrying on a fresh connection cannot duplicate work.
func staleErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "connection reset") || strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "forcibly closed") || strings.Contains(s, "connection aborted")
}

// Do sends r and returns the response with its headers read. The caller owns
// resp.Body and must Close it; reading it to EOF returns the connection to the
// pool. Cancelling ctx aborts the exchange (closing the connection).
func (w *Wire) Do(ctx context.Context, r Request) (*http.Response, error) {
	scheme, host, port, hostHeader, selector, err := splitTarget(r.URL)
	if err != nil {
		return nil, err
	}
	if w.Proxy != nil && scheme == "http" {
		selector = r.URL
	}
	key := scheme + "://" + net.JoinHostPort(host, port)
	retried := false
	for {
		c := w.takeIdle(key)
		reused := c != nil
		if c == nil {
			raw, derr := w.dial(ctx, scheme, host, port)
			if derr != nil {
				return nil, derr
			}
			c = &wireConn{Conn: raw, br: bufio.NewReaderSize(raw, 32<<10), key: key}
		}
		resp, err := w.exchange(ctx, c, r, hostHeader, selector, reused)
		if err != nil {
			c.Close()
			if reused && !retried && staleErr(err) && ctx.Err() == nil {
				retried = true
				continue
			}
			return nil, err
		}
		return resp, nil
	}
}

func (w *Wire) buildRequest(r Request, hostHeader, selector string) []byte {
	var b bytes.Buffer
	method := r.Method
	if method == "" {
		method = http.MethodPost
	}
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\n", method, selector)
	has := func(name string) bool { _, ok := r.Headers.Get(name); return ok }
	// Same wire shape as the legacy gateway (CPython http.client): the client
	// adds Accept-Encoding and Content-Length first, then Host, then the
	// caller's headers in order, then Connection.
	if !has("Accept-Encoding") {
		b.WriteString("Accept-Encoding: identity\r\n")
	}
	if r.Body != nil && !has("Content-Length") {
		fmt.Fprintf(&b, "Content-Length: %d\r\n", len(r.Body))
	}
	if !has("Host") {
		fmt.Fprintf(&b, "Host: %s\r\n", hostHeader)
	}
	for _, p := range r.Headers {
		// Header injection guard: values never carry line breaks.
		if strings.ContainsAny(p.Name, "\r\n:") || strings.ContainsAny(p.Value, "\r\n") {
			continue
		}
		b.WriteString(p.Name)
		b.WriteString(": ")
		b.WriteString(p.Value)
		b.WriteString("\r\n")
	}
	b.WriteString("Connection: keep-alive\r\n\r\n")
	b.Write(r.Body)
	return b.Bytes()
}

func (w *Wire) exchange(ctx context.Context, c *wireConn, r Request, hostHeader, selector string, reused bool) (*http.Response, error) {
	// Abort on cancellation by closing the socket; the read below then fails.
	stop := context.AfterFunc(ctx, func() { c.Close() })
	hdrTimeout := w.T.ResponseHeader
	if r.HeaderTimeout > 0 {
		hdrTimeout = r.HeaderTimeout
	}
	if hdrTimeout > 0 {
		_ = c.SetDeadline(time.Now().Add(hdrTimeout))
	}
	if _, err := c.Write(w.buildRequest(r, hostHeader, selector)); err != nil {
		stop()
		return nil, err
	}
	method := r.Method
	if method == "" {
		method = http.MethodPost
	}
	resp, err := http.ReadResponse(c.br, &http.Request{Method: method})
	if err != nil {
		stop()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	_ = c.SetDeadline(time.Time{})
	resp.Body = &wireBody{ReadCloser: resp.Body, w: w, c: c, stop: stop, reusable: !resp.Close}
	return resp, nil
}

// wireBody hands the connection back to the pool once the body was read to its
// end; any other way of finishing discards the connection.
type wireBody struct {
	io.ReadCloser
	w        *Wire
	c        *wireConn
	stop     func() bool
	reusable bool
	eof      bool
	done     bool
}

func (b *wireBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.eof = true
	}
	return n, err
}

func (b *wireBody) Close() error {
	if b.done {
		return nil
	}
	b.done = true
	_ = b.ReadCloser.Close()
	// stop() reports false when the cancel hook already ran, i.e. the socket
	// was closed under us and must not be pooled.
	alive := b.stop()
	if b.eof && b.reusable && alive && b.c.br.Buffered() == 0 {
		b.w.putIdle(b.c)
		return nil
	}
	return b.c.Close()
}
