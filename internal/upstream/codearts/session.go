package codearts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"wb2api/internal/upstream/identity"
)

// HeartbeatPath registers a chat session slot. The upstream allows three
// concurrent conversations per account; the fourth answers TM.00001041. Slots
// are not tied to a request: they are busy/idle leases, so a chat request must
// (a) register busy first, (b) carry the same user-session-id, (c) renew every
// 30 s, and (d) send idle when the conversation ends.
const HeartbeatPath = "/snap-manager/v1/chat-session/heartbeat"

// HeartbeatInterval is the renewal period of a busy lease.
const HeartbeatInterval = 30 * time.Second

// HeartbeatError is a response rejecting the proposed lease change.
type HeartbeatError struct {
	Status int
	Body   []byte
}

func (e *HeartbeatError) Error() string {
	return fmt.Sprintf("codearts heartbeat rejected (HTTP %d)", e.Status)
}

// SessionStatus values accepted by the heartbeat endpoint.
const (
	SessionBusy = "busy"
	SessionIdle = "idle"
)

// NewUserSessionID returns a 32-hex-character id (a UUIDv4 without dashes).
// Never reuse one: releasing another request's id would kick its conversation.
func NewUserSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[:]), nil
}

// Heartbeat sends one busy/idle lease change.
//
// The signature covers Host as well; the header set is the official client's,
// so nothing may be added or removed.
func Heartbeat(ctx context.Context, hc *http.Client, base string, cred Credential, sessionID, status string) error {
	trace, err := NewUserSessionID()
	if err != nil {
		return err
	}
	url := strings.TrimRight(base, "/") + HeartbeatPath + "?status=" + status
	body := []byte("{}")
	h := identity.Headers{
		identity.P("Content-Type", "application/json"),
		identity.P("Accept", "application/json"),
		identity.P("X-Language", "zh-cn"),
		identity.P("x-client-type", "kernel"),
		identity.P("user-session-id", sessionID),
		identity.P("x-snap-traceid", trace),
	}
	h = Signer{}.SignHeaders(h, http.MethodPut, url, body, cred)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader("{}"))
	if err != nil {
		return err
	}
	for _, p := range h {
		if strings.EqualFold(p.Name, "Host") {
			continue
		}
		req.Header.Set(p.Name, p.Value)
	}
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("codearts heartbeat: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("codearts heartbeat: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return &HeartbeatError{Status: resp.StatusCode, Body: raw}
	}
	if !heartbeatAccepted(raw) {
		status := http.StatusBadGateway
		if fault := ParseChatError(raw); fault != nil {
			status = fault.Status
		}
		return &HeartbeatError{Status: status, Body: raw}
	}
	return nil
}

// heartbeatAccepted reports whether the upstream confirmed the lease. Anything
// other than {"status":"ok"} is a failure, no matter what else the body says.
func heartbeatAccepted(raw []byte) bool {
	var out struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(out.Status), "ok")
}

// IsSessionLimitError reports the upstream's "this account already has three
// conversations" rejection.
func IsSessionLimitError(body []byte) bool {
	s := string(body)
	return strings.Contains(s, "TM.00001041") || strings.Contains(s, "session") && strings.Contains(s, "limit")
}
