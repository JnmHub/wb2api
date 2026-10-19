// Package upstream builds the outbound HTTP machinery. The key rule inherited
// from the legacy gateway: every account gets its own connection pool, so two
// accounts never share a TCP/TLS connection (the upstream must not be able to
// correlate them through a shared connection).
package upstream

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Timeouts are the layered limits applied to every upstream call. There is no
// total-duration cap: a healthy stream may run for minutes.
type Timeouts struct {
	Dial           time.Duration // TCP connect
	TLS            time.Duration // TLS handshake
	ResponseHeader time.Duration // request sent -> response headers (first byte)
	Idle           time.Duration // max silence between two reads of a stream
}

// DefaultTimeouts are tighter than the legacy 120 s header timeout so that a
// hung account or exit fails over in tens of seconds, not minutes.
func DefaultTimeouts() Timeouts {
	return Timeouts{Dial: 8 * time.Second, TLS: 10 * time.Second, ResponseHeader: 45 * time.Second, Idle: 300 * time.Second}
}

// Manager hands out one *http.Client per (account, proxy) pair.
type Manager struct {
	T Timeouts

	mu      sync.RWMutex
	clients map[string]*http.Client
}

// NewManager creates a Manager with the given timeouts.
func NewManager(t Timeouts) *Manager {
	return &Manager{T: t, clients: map[string]*http.Client{}}
}

// Client returns the dedicated client for an account. proxy may be empty
// (direct) or an http(s) proxy URL.
func (m *Manager) Client(uid, proxy string) *http.Client {
	key := uid + "|" + strings.TrimSpace(proxy)
	m.mu.RLock()
	c := m.clients[key]
	m.mu.RUnlock()
	if c != nil {
		return c
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c = m.clients[key]; c != nil {
		return c
	}
	c = &http.Client{Transport: m.newTransport(proxy)}
	m.clients[key] = c
	return c
}

func (m *Manager) newTransport(proxy string) *http.Transport {
	d := &net.Dialer{Timeout: m.T.Dial, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return d.DialContext(ctx, network, addr)
		},
		TLSHandshakeTimeout:   m.T.TLS,
		ResponseHeaderTimeout: m.T.ResponseHeader,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		// HTTP/1.1 only: the legacy gateway speaks HTTP/1.1 and an h2 session
		// would multiplex several requests of one account on a single stream
		// set, which also changes what the upstream observes.
		ForceAttemptHTTP2:  false,
		DisableCompression: true,
	}
	tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{} // disables h2
	if p := strings.TrimSpace(proxy); p != "" {
		if u, err := url.Parse(p); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return tr
}

// Drop discards the clients of an account (e.g. after its proxy changed) and
// closes their idle connections.
func (m *Manager) Drop(uid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, c := range m.clients {
		if strings.HasPrefix(k, uid+"|") {
			if tr, ok := c.Transport.(*http.Transport); ok {
				tr.CloseIdleConnections()
			}
			delete(m.clients, k)
		}
	}
}

// CloseIdle closes idle connections of every client.
func (m *Manager) CloseIdle() {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, c := range m.clients {
		if tr, ok := c.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}
}
