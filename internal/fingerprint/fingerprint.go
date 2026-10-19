// Package fingerprint derives the stable per-account device identifiers the
// upstream expects. It is a byte-for-byte port of legacy/python/wb_fingerprint.py:
// every account must keep presenting the same "device" it always did, so the
// algorithm (MD5 over "salt:uid", UUIDv4-shaped) must not drift.
package fingerprint

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
)

// DeriveID returns a hyphenated, UUIDv4-shaped identifier derived from uid and
// salt. It is idempotent: the same inputs always produce the same value.
func DeriveID(uid, salt string) string {
	if uid == "" {
		uid = "anonymous"
	}
	sum := md5.Sum([]byte(salt + ":" + uid))
	h := []byte(hex.EncodeToString(sum[:])) // 32 hex chars
	// version nibble (index 12) = '4', variant nibble (index 16) = 'a'.
	h[12] = '4'
	h[16] = 'a'
	s := string(h)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// MachineID is the per-account machine identifier.
func MachineID(uid string) string { return DeriveID(uid, "machine") }

// SessionID is the per-account (not per-process) session identifier.
func SessionID(uid string) string { return DeriveID(uid, "session") }

// NewRequestID returns a random 32-char hex request id (X-Request-ID shape).
func NewRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
