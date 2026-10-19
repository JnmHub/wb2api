package openai

import (
	"context"
	"crypto/x509"
	"errors"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"
)

// Markers of "this machine has no network": DNS failed, no route, interface
// down. The account never sent a byte, so it must not be charged for it.
var localNetMarkers = []string{
	"no such host", "getaddrinfo", "name or service not known", "nodename nor servname",
	"temporary failure in name resolution", "network is unreachable", "network is down",
	"no route to host", "winerror 11001", "winerror 10050", "winerror 10051", "winerror 10065",
	"the requested name is valid, but no data", "a socket operation was attempted to an unreachable",
}

func isLocalNetwork(err error) bool {
	if err == nil {
		return false
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return !dns.IsTimeout // a lookup that failed outright; a slow resolver is a timeout
	}
	t := strings.ToLower(err.Error())
	for _, m := range localNetMarkers {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}

// isTLSVerify: certificate/hostname verification failures are deterministic —
// every retry hits the same chain, so one attempt is enough.
func isTLSVerify(err error) bool {
	var ua x509.UnknownAuthorityError
	var ci x509.CertificateInvalidError
	var he x509.HostnameError
	return errors.As(err, &ua) || errors.As(err, &ci) || errors.As(err, &he) ||
		strings.Contains(strings.ToLower(err.Error()), "certificate signed by unknown authority")
}

// Network-level flakiness that deserves a retry, not a cooldown (a dropped
// handshake, a reset, a timeout).
var transientMarkers = []string{
	"ssl", "tls", "unexpected eof", "eof", "remote end closed", "connection reset", "connection aborted",
	"broken pipe", "timed out", "timeout", "temporarily unavailable", "bad gateway", "502", "503", "504",
	"incomplete", "forcibly closed", "use of closed",
}

func isTransient(err error) bool {
	if err == nil || isTLSVerify(err) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	t := strings.ToLower(err.Error())
	for _, m := range transientMarkers {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- egress

// Egress-level transient accounting (legacy wb_egress): a few seconds of link
// flakiness is charged to the outbound path, not to every account, so it earns
// a seconds-level backoff instead of N half-hour breakers.
const (
	egressBase        = time.Second
	egressMax         = 15 * time.Second
	egressStreakReset = 60 * time.Second
	egressSleepMax    = 5 * time.Second
)

type egressTracker struct {
	mu    sync.Mutex
	state map[string]*egressState
}

type egressState struct {
	streak int
	last   time.Time
}

func egressBackoff(streak int) time.Duration {
	step := streak - 1
	if step < 0 {
		step = 0
	}
	if step > 20 {
		step = 20
	}
	d := egressBase << uint(step)
	if d > egressMax || d <= 0 {
		d = egressMax
	}
	return d
}

// note records one transient failure on a path and returns the backoff (with
// 0.75–1.25x jitter so retries do not synchronise).
func (e *egressTracker) note(key string, now time.Time) time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state == nil {
		e.state = map[string]*egressState{}
	}
	st := e.state[key]
	if st == nil || now.Sub(st.last) > egressStreakReset {
		st = &egressState{}
		e.state[key] = st
	}
	st.streak++
	st.last = now
	return time.Duration(float64(egressBackoff(st.streak)) * (0.75 + rand.Float64()*0.5))
}

func (e *egressTracker) success(key string) {
	e.mu.Lock()
	delete(e.state, key)
	e.mu.Unlock()
}

// sleepCtx waits d or until the request is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
