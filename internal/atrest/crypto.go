// Package atrest decrypts the desktop client's at-rest credential envelopes.
// It is a port of legacy wb_atrest.py: AES-256-GCM with a byte-exact AAD, a
// keyblob that wraps the master key, and a memory-only key cache. The key hunt
// through the client's process memory is not ported (see HuntHint); an operator
// can paste the 64-hex protector key or the 44-character base64 payload secret.
package atrest

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func randRead(b []byte) (int, error) { return rand.Read(b) }

const (
	EnvelopeKey = "$wbEncrypted"
	Scheme      = "sym-v1"
	Suite       = 1
)

var (
	framingMagic = map[string]string{"file": "WBEF1", "field": "WBEV1", "record": "WBER1", "stream": "WBES1"}
	framingCode  = map[string]byte{"file": 1, "field": 2, "record": 3, "stream": 4}
)

// DeriveKey turns the 44-character base64 atRestSecretKey into the 32-byte
// protector key: sha256 over the *text*, not over its decoded bytes.
func DeriveKey(secret string) []byte {
	h := sha256.Sum256([]byte(secret))
	return h[:]
}

// KeyID is the first 16 hex characters of sha256(key).
func KeyID(key []byte) string {
	h := sha256.Sum256(key)
	return hex.EncodeToString(h[:])[:16]
}

func appendLP(b []byte, s string) []byte {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(s)))
	b = append(b, n[:]...)
	return append(b, s...)
}

// aad is byte-exact with the client: WB-AAD\0, version 1, length-prefixed
// framing magic, scheme, big-endian suite, key id, framing code, 0, 0.
func aad(framing, keyID string, suite int) ([]byte, error) {
	magic, ok := framingMagic[framing]
	if !ok {
		return nil, fmt.Errorf("unknown framing %q", framing)
	}
	code := framingCode[framing]
	b := []byte("WB-AAD\x00")
	b = append(b, 0x01)
	b = appendLP(b, magic)
	b = appendLP(b, Scheme)
	var u [4]byte
	binary.BigEndian.PutUint32(u[:], uint32(suite))
	b = append(b, u[:]...)
	b = appendLP(b, keyID)
	b = append(b, code, 0x00, 0x00)
	return b, nil
}

// IsEnvelope reports whether a field holds an at-rest envelope (or the legacy
// JSON-in-a-string form).
func IsEnvelope(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		_, hasKey := t[EnvelopeKey]
		env, _ := t["envelope"].(string)
		return hasKey && env != ""
	case string:
		s := strings.TrimLeft(t, " \t\r\n")
		return strings.HasPrefix(s, "{") && strings.Contains(s[:min(len(s), 64)], EnvelopeKey)
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type envelope struct {
	Suite      int    `json:"suite"`
	KeyID      string `json:"keyId"`
	Nonce      string `json:"nonce"`
	AuthTag    string `json:"authTag"`
	Ciphertext string `json:"ciphertext"`
}

func parseEnvelope(v any) (*envelope, error) {
	var raw string
	switch t := v.(type) {
	case map[string]any:
		raw, _ = t["envelope"].(string)
	case string:
		raw = t
	default:
		return nil, errors.New("envelope is not a string")
	}
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "{") { // old form: the whole envelope JSON in a string
		var outer struct {
			Envelope string `json:"envelope"`
		}
		if err := json.Unmarshal([]byte(raw), &outer); err == nil && outer.Envelope != "" {
			raw = outer.Envelope
		}
	}
	blob, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("envelope is not base64(JSON): %w", err)
	}
	var env envelope
	if err := json.Unmarshal(blob, &env); err != nil {
		return nil, fmt.Errorf("envelope JSON: %w", err)
	}
	if env.Suite == 0 {
		env.Suite = Suite
	}
	if env.Nonce == "" || env.Ciphertext == "" {
		return nil, errors.New("envelope lacks nonce/ciphertext")
	}
	return &env, nil
}

func gcm(key []byte, nonce []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) == 12 {
		return cipher.NewGCM(block)
	}
	return cipher.NewGCMWithNonceSize(block, len(nonce))
}

// OpenEnvelope decrypts one envelope with the given framing.
func OpenEnvelope(key []byte, value any, framing string) ([]byte, error) {
	env, err := parseEnvelope(value)
	if err != nil {
		return nil, err
	}
	expect := KeyID(key)
	if env.KeyID != "" && env.KeyID != expect {
		return nil, fmt.Errorf("envelope keyId %s does not match the key (%s)", env.KeyID, expect)
	}
	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	tag, err := base64.StdEncoding.DecodeString(env.AuthTag)
	if err != nil {
		return nil, fmt.Errorf("authTag: %w", err)
	}
	ct, err := base64.StdEncoding.DecodeString(env.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("ciphertext: %w", err)
	}
	keyID := env.KeyID
	if keyID == "" {
		keyID = expect
	}
	extra, err := aad(framing, keyID, env.Suite)
	if err != nil {
		return nil, err
	}
	aead, err := gcm(key, nonce)
	if err != nil {
		return nil, err
	}
	sealed := append(append([]byte(nil), ct...), tag...)
	plain, err := aead.Open(nil, nonce, sealed, extra)
	if err != nil {
		return nil, errors.New("envelope authentication failed (wrong key or tampered data)")
	}
	return plain, nil
}

// DecryptField returns the plaintext of a possibly-encrypted field. ok is false
// for a non-string, non-envelope value (the caller may keep a fallback).
func DecryptField(key []byte, value any) (string, bool) {
	if value == nil || value == "" {
		return "", true
	}
	if !IsEnvelope(value) {
		s, ok := value.(string)
		return s, ok
	}
	plain, err := OpenEnvelope(key, value, "field")
	if err != nil {
		return "", false
	}
	return string(plain), true
}

// Seal builds an envelope with the given key and framing (tests and fixtures).
func Seal(key []byte, plain []byte, framing string) (map[string]any, error) {
	nonce := make([]byte, 12)
	if _, err := randRead(nonce); err != nil {
		return nil, err
	}
	extra, err := aad(framing, KeyID(key), Suite)
	if err != nil {
		return nil, err
	}
	aead, err := gcm(key, nonce)
	if err != nil {
		return nil, err
	}
	sealed := aead.Seal(nil, nonce, plain, extra)
	ct, tag := sealed[:len(sealed)-16], sealed[len(sealed)-16:]
	env := envelope{Suite: Suite, KeyID: KeyID(key),
		Nonce: base64.StdEncoding.EncodeToString(nonce), AuthTag: base64.StdEncoding.EncodeToString(tag),
		Ciphertext: base64.StdEncoding.EncodeToString(ct)}
	b, _ := json.Marshal(env)
	return map[string]any{EnvelopeKey: Suite, "envelope": base64.StdEncoding.EncodeToString(b)}, nil
}

// EnvelopeString is the base64 envelope a field would carry.
func EnvelopeString(key []byte, plain []byte, framing string) (string, error) {
	env, err := Seal(key, plain, framing)
	if err != nil {
		return "", err
	}
	return env["envelope"].(string), nil
}
