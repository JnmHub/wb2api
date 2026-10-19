package settings

import (
	"os"
	"strings"
	"sync"
	"time"
)

// Defaults of the "upstream" settings group (identical to the legacy values).
const (
	DefaultHeaderTimeoutSeconds = 120
	DefaultIdleTimeoutSeconds   = 300
)

// UpstreamConfig is the "upstream" settings group.
type UpstreamConfig struct {
	HeaderTimeout   time.Duration
	IdleTimeout     time.Duration
	DeviceToken     string
	DeviceTokenFile string
}

func positiveInt(v any, def int) int {
	switch t := v.(type) {
	case bool:
		return def
	default:
		n := intOf(t)
		if n >= 1 {
			if _, isStr := v.(string); isStr {
				return def // the legacy loader only accepts real integers
			}
			return n
		}
	}
	return def
}

func trimmedString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// UpstreamConfig returns the upstream group with legacy defaults and validation.
func (s *Store) UpstreamConfig() UpstreamConfig {
	g, _ := s.View()["upstream"].(map[string]any)
	return UpstreamConfig{
		HeaderTimeout:   time.Duration(positiveInt(g["header_timeout_seconds"], DefaultHeaderTimeoutSeconds)) * time.Second,
		IdleTimeout:     time.Duration(positiveInt(g["idle_timeout_seconds"], DefaultIdleTimeoutSeconds)) * time.Second,
		DeviceToken:     trimmedString(g["device_token"]),
		DeviceTokenFile: trimmedString(g["device_token_file"]),
	}
}

// PromptConfig is the gateway system-prompt mode.
type PromptConfig struct {
	Mode string // passthrough | custom | append
	File string
}

// PromptConfig returns the prompt group; anything invalid reads as passthrough.
func (s *Store) PromptConfig() PromptConfig {
	g, _ := s.View()["prompt"].(map[string]any)
	mode := "passthrough"
	if m, ok := g["mode"].(string); ok {
		switch lm := strings.ToLower(strings.TrimSpace(m)); lm {
		case "passthrough", "custom", "append":
			mode = lm
		}
	}
	return PromptConfig{Mode: mode, File: trimmedString(g["file"])}
}

type deviceTokenCache struct {
	mu    sync.Mutex
	key   string
	token string
	at    time.Time
}

const (
	deviceTokenTTL      = 5 * time.Minute
	deviceTokenMaxBytes = 1024
)

// ResolveDeviceToken returns the optional X-Device-Token: a literal value or the
// content of a token file (at most 1 KiB, trimmed), cached for five minutes.
// Any failure degrades to an empty token.
func (s *Store) ResolveDeviceToken() string {
	cfg := s.UpstreamConfig()
	key := cfg.DeviceToken
	if key == "" {
		key = cfg.DeviceTokenFile
	}
	if key == "" {
		return ""
	}
	c := &s.dev
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.key == key && time.Since(c.at) < deviceTokenTTL {
		return c.token
	}
	token := cfg.DeviceToken
	if token == "" && cfg.DeviceTokenFile != "" {
		if st, err := os.Stat(cfg.DeviceTokenFile); err == nil && st.Size() <= deviceTokenMaxBytes {
			if b, err := os.ReadFile(cfg.DeviceTokenFile); err == nil {
				token = strings.TrimSpace(string(b))
			}
		}
	}
	c.key, c.token, c.at = key, token, time.Now()
	return token
}
