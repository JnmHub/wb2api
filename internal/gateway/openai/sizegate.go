package openai

import (
	"sync"
	"time"
)

// The Huawei channel rejects oversized requests with a wasted round trip: the
// inference backend answers InferHub.001001005 around 6 MiB, the API gateway
// answers HTTP 413 APIG.0201 around 12 MiB. Neither advertises the limit, so a
// measured rejection is remembered per account and same-or-larger requests skip
// that account until the ceiling expires.
const (
	// sizeGateMinBytes is the smallest body that can plausibly be a size
	// rejection: 001001005 is also the generic "invalid parameter" code, and a
	// small body with that code is a real client error, not a size wall.
	sizeGateMinBytes = 4 << 20
	// sizeGateTTL bounds how long a measured wall is trusted.
	sizeGateTTL = 120 * time.Second
)

type sizeWall struct {
	bytes  int
	until  time.Time
	reason string
}

// sizeGate remembers measured request-size walls per account.
type sizeGate struct {
	mu sync.Mutex
	m  map[string]sizeWall
}

// blocked reports whether sending size bytes to uid would hit a remembered
// wall.
func (g *sizeGate) blocked(uid string, size int, now time.Time) (bool, string) {
	if g == nil || size < sizeGateMinBytes {
		return false, ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	w, ok := g.m[uid]
	if !ok || now.After(w.until) {
		return false, ""
	}
	if size >= w.bytes {
		return true, w.reason
	}
	return false, ""
}

// record stores a measured wall: this many bytes did not get through.
func (g *sizeGate) record(uid string, size int, now time.Time, reason string) {
	if g == nil || size < sizeGateMinBytes {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = map[string]sizeWall{}
	}
	g.m[uid] = sizeWall{bytes: size, until: now.Add(sizeGateTTL), reason: reason}
}

// sizeWallReason explains a skip for the logs.
func (g *sizeGate) forget(uid string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.m, uid)
}
