package atrest

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const keyblobName = "keyblob"

var configCandidates = []string{".workbuddy-ai", ".workbuddy"}

var (
	keyHexRE = regexp.MustCompile(`(?i)^[0-9a-f]{64}$`)
	keyB64RE = regexp.MustCompile(`^[A-Za-z0-9+/]{43}=$`)
)

// Keyring owns the keyblob location and the in-memory protector key.
type Keyring struct {
	// Dir overrides the config directory (tests).
	Dir string
	// BlobPath overrides the keyblob file (tests).
	BlobPath string

	mu     sync.Mutex
	key    []byte
	source string
}

func (k *Keyring) configDir() string {
	if k.Dir != "" {
		return k.Dir
	}
	if env := os.Getenv("WORKBUDDY_CONFIG_DIR"); env != "" {
		return filepath.Clean(env)
	}
	if env := os.Getenv("CODEBUDDY_CONFIG_DIR"); env != "" {
		return filepath.Clean(env)
	}
	home, _ := os.UserHomeDir()
	for _, name := range configCandidates {
		dir := filepath.Join(home, name)
		if _, err := os.Stat(filepath.Join(dir, keyblobName)); err == nil {
			return dir
		}
	}
	return filepath.Join(home, configCandidates[0])
}

// BlobFile is the keyblob the keyring seals against.
func (k *Keyring) BlobFile() string {
	if k.BlobPath != "" {
		return k.BlobPath
	}
	return filepath.Join(k.configDir(), keyblobName)
}

type keySlot struct {
	Type           string `json:"type"`
	ProtectorKeyID string `json:"protectorKeyId"`
	Wrapped        any    `json:"wrapped"`
}

type keyBlob struct {
	KeyID string    `json:"keyId"`
	Slots []keySlot `json:"slots"`
}

// LoadBlob reads the client's keyblob.
func (k *Keyring) LoadBlob() (*keyBlob, error) {
	path := k.BlobFile()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("keyblob 不存在或不可读：%s（桌面客户端没装或没登录过？）", path)
	}
	var blob keyBlob
	if err := json.Unmarshal(raw, &blob); err != nil {
		return nil, fmt.Errorf("keyblob 读取失败：%w", err)
	}
	return &blob, nil
}

// Unwrap validates a protector key against the keyblob and returns the master.
func (k *Keyring) Unwrap(key []byte) ([]byte, error) {
	blob, err := k.LoadBlob()
	if err != nil {
		return nil, err
	}
	want := KeyID(key)
	for _, slot := range blob.Slots {
		if slot.Type != "static-v1" || slot.Wrapped == nil {
			continue
		}
		if strings.ToLower(slot.ProtectorKeyID) != want {
			return nil, errors.New("密钥与 keyblob 的 protectorKeyId 不匹配")
		}
		master, err := OpenEnvelope(key, slot.Wrapped, "file")
		if err != nil {
			return nil, err
		}
		if len(master) != 32 || KeyID(master) != strings.ToLower(blob.KeyID) {
			return nil, errors.New("keyblob 解出的主密钥校验失败")
		}
		return master, nil
	}
	return nil, errors.New("keyblob 里没有可用的 static-v1 槽")
}

// ParseKeyText accepts a 64-hex protector key or the 44-character base64
// payload secret (sha256 of the text).
func ParseKeyText(text string) ([]byte, error) {
	raw := strings.TrimSpace(text)
	if strings.HasPrefix(strings.ToLower(raw), "0x") {
		raw = strings.TrimSpace(raw[2:])
	}
	if keyHexRE.MatchString(raw) {
		key, err := hex.DecodeString(raw)
		if err != nil {
			return nil, err
		}
		return key, nil
	}
	if keyB64RE.MatchString(raw) {
		if _, err := base64.StdEncoding.DecodeString(raw); err != nil {
			return nil, fmt.Errorf("base64 解码失败：%w", err)
		}
		return DeriveKey(raw), nil
	}
	return nil, errors.New("认不出密钥格式：需要 64 位 hex 保护密钥，或 44 字符 base64 的 atRestSecretKey")
}

// Adopt parses and validates a manually supplied key, then caches it in memory.
func (k *Keyring) Adopt(text string) ([]byte, error) {
	key, err := ParseKeyText(text)
	if err != nil {
		return nil, err
	}
	if _, err := k.Unwrap(key); err != nil {
		return nil, err
	}
	k.mu.Lock()
	k.key, k.source = key, "manual"
	k.mu.Unlock()
	return key, nil
}

// Cached returns the cached key, revalidating it against the keyblob so a
// client-side key rotation invalidates it immediately.
func (k *Keyring) Cached() []byte {
	k.mu.Lock()
	key := k.key
	k.mu.Unlock()
	if key == nil {
		return nil
	}
	if _, err := k.Unwrap(key); err != nil {
		k.Forget()
		return nil
	}
	return key
}

// Forget drops the cached key.
func (k *Keyring) Forget() {
	k.mu.Lock()
	k.key, k.source = nil, ""
	k.mu.Unlock()
}

// HuntSupported is false: the Windows process-memory hunt is not ported.
func HuntSupported() bool { return false }

// HuntHint explains how to supply the key by hand instead.
func HuntHint() string {
	return "自动回收桌面客户端内存中的密钥尚未移植；请在 workbuddy 客户端同机用 64 位 hex 保护密钥或 44 字符 base64 的 atRestSecretKey 手工导入。"
}

// Status describes the keyring without exposing key material.
func (k *Keyring) Status() map[string]any {
	out := map[string]any{
		"platform": os.Getenv("OS"), "hunt_supported": HuntSupported(), "hunt_hint": HuntHint(),
		"keyblob": k.BlobFile(), "keyblob_found": false, "key_id": "", "key_cached": false, "key_source": "", "error": "",
	}
	blob, err := k.LoadBlob()
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	out["keyblob_found"] = true
	out["key_id"] = strings.ToLower(blob.KeyID)
	k.mu.Lock()
	key, source := k.key, k.source
	k.mu.Unlock()
	out["key_cached"] = key != nil
	out["key_source"] = source
	return out
}
