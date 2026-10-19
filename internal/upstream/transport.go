// Package upstream builds the outbound HTTP machinery. The key rule inherited
// from the legacy gateway: every account gets its own connection pool, so two
// accounts never share a TCP/TLS connection (the upstream must not be able to
// correlate them through a shared connection).
package upstream

import (
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

// DefaultTimeouts keep the legacy 120 s wait for the first response byte (a
// reasoning model can be silent that long) but fail a dead host or proxy fast:
// the connect and TLS steps have their own short limits.
func DefaultTimeouts() Timeouts {
	return Timeouts{Dial: 8 * time.Second, TLS: 10 * time.Second, ResponseHeader: 120 * time.Second, Idle: 300 * time.Second}
}

// Manager hands out one *Wire per (account, proxy) pair.
type Manager struct {
	T Timeouts

	mu    sync.RWMutex
	wires map[string]*Wire
}

// NewManager creates a Manager with the given timeouts.
func NewManager(t Timeouts) *Manager {
	return &Manager{T: t, wires: map[string]*Wire{}}
}

// Wire returns the dedicated client for an account. proxy may be empty (direct)
// or an http(s) proxy URL.
func (m *Manager) Wire(uid, proxy string) *Wire {
	proxy = strings.TrimSpace(proxy)
	key := uid + "|" + proxy
	m.mu.RLock()
	w := m.wires[key]
	m.mu.RUnlock()
	if w != nil {
		return w
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if w = m.wires[key]; w != nil {
		return w
	}
	var pu *url.URL
	if proxy != "" {
		if u, err := url.Parse(proxy); err == nil && u.Host != "" {
			pu = u
		} else if u, err := url.Parse("http://" + proxy); err == nil && u.Host != "" {
			pu = u // authority form host:port
		}
	}
	w = NewWire(m.T, pu)
	m.wires[key] = w
	return w
}

// Drop discards the wires of an account (e.g. after its proxy changed) and
// closes their idle connections.
func (m *Manager) Drop(uid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, w := range m.wires {
		if strings.HasPrefix(k, uid+"|") {
			w.CloseIdle()
			delete(m.wires, k)
		}
	}
}

// CloseIdle closes idle connections of every wire.
func (m *Manager) CloseIdle() {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, w := range m.wires {
		w.CloseIdle()
	}
}
