package panelauth

import (
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
)

// TrustedProxies are the peers allowed to speak for their clients: loopback
// (a reverse proxy on the same machine) plus WB_TRUSTED_PROXIES (comma
// separated IPs / CIDRs; unparsable entries are dropped, a typo must not stop
// the gateway from starting).
type TrustedProxies []netip.Prefix

// ParseTrustedProxies parses a WB_TRUSTED_PROXIES value.
func ParseTrustedProxies(raw string) TrustedProxies {
	var out TrustedProxies
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if p, err := netip.ParsePrefix(item); err == nil {
			out = append(out, p.Masked())
		} else if a, err := netip.ParseAddr(item); err == nil {
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	return out
}

// TrustedProxiesFromEnv reads WB_TRUSTED_PROXIES.
func TrustedProxiesFromEnv() TrustedProxies {
	return ParseTrustedProxies(os.Getenv("WB_TRUSTED_PROXIES"))
}

// Trusted reports whether peer may speak for its clients.
func (t TrustedProxies) Trusted(peer string) bool {
	a, err := netip.ParseAddr(peer)
	if err != nil {
		return false
	}
	a = a.Unmap() // ::ffff:127.0.0.1 is loopback too
	if a.IsLoopback() {
		return true
	}
	for _, p := range t {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// headerIP extracts one valid IP from a proxy header value, accepting bare
// IPv4/IPv6, host:port and [v6]:port. Multi-valued, over-long and malformed
// values yield "".
func headerIP(value string) string {
	text := strings.TrimSpace(value)
	if text == "" || len(text) > 64 || strings.Contains(text, ",") {
		return ""
	}
	if strings.HasPrefix(text, "[") {
		end := strings.Index(text, "]")
		if end == -1 {
			return ""
		}
		text = text[1:end]
	} else if strings.Count(text, ":") == 1 {
		host, port, _ := strings.Cut(text, ":")
		if port != "" && strings.Trim(port, "0123456789") == "" {
			text = host
		}
	}
	a, err := netip.ParseAddr(text)
	if err != nil {
		return ""
	}
	return a.String()
}

// PeerIP returns the socket peer's IP without the port.
func PeerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return a.Unmap().String()
	}
	return host
}

// ClientKey is the rate-limit bucket of a request: the real client address when
// the peer is a trusted proxy (X-Real-IP first, then the first X-Forwarded-For
// entry), otherwise the peer itself - an untrusted client cannot pick another
// bucket by forging headers.
func (t TrustedProxies) ClientKey(r *http.Request) string {
	peer := PeerIP(r)
	if !t.Trusted(peer) {
		return peer
	}
	if ip := headerIP(r.Header.Get("X-Real-IP")); ip != "" {
		return ip
	}
	first, _, _ := strings.Cut(r.Header.Get("X-Forwarded-For"), ",")
	if ip := headerIP(first); ip != "" {
		return ip
	}
	return peer
}

// IsHTTPS reports whether the client talked TLS to us or to a trusted proxy.
func (t TrustedProxies) IsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return t.Trusted(PeerIP(r)) && strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}
