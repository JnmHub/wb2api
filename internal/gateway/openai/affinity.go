package openai

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strconv"
	"strings"

	"wb2api/internal/jsonx"
	"wb2api/internal/pool"
)

// Prefix affinity. The upstream prompt cache is per account, so a conversation
// only benefits when its turns land on the same account. Many clients send no
// session id, so the key is derived from the conversation's stable head (the
// first two messages): every turn agrees on key and page with no server state.
type affinityConfig struct {
	byPrefix bool
	maxMsgs  int // past this many messages a conversation stops pinning (0 = never)
	pageSize int // >=2: rotate over a page of the pool; 1: pin anyway; 0: whole pool
}

func envInt(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func affinityFromEnv() affinityConfig {
	on := true
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WB_AFFINITY_BY_PREFIX"))) {
	case "0", "false", "no", "off":
		on = false
	}
	return affinityConfig{byPrefix: on, maxMsgs: envInt("WB_AFFINITY_MAX_MSGS", 400), pageSize: envInt("WB_AFFINITY_PAGE_SIZE", 3)}
}

// route returns the conversation key and, for conversations past the length
// cap, the pool page they rotate over.
func (c affinityConfig) route(messages []any) (string, *pool.Page) {
	if !c.byPrefix || len(messages) == 0 {
		return "", nil
	}
	head := messages
	if len(head) > 2 {
		head = head[:2]
	}
	blob, err := jsonx.MarshalPySorted(head)
	if err != nil {
		return "", nil
	}
	sum := sha256.Sum256(blob)
	digest := hex.EncodeToString(sum[:])[:16]
	key := "pfx-" + digest
	if c.maxMsgs > 0 && len(messages) > c.maxMsgs {
		switch {
		case c.pageSize > 1:
			n, _ := strconv.ParseUint(digest[:8], 16, 64)
			return key, &pool.Page{Index: int(n % uint64(c.pageSize)), Size: c.pageSize}
		case c.pageSize == 1:
			return key, nil
		}
		return "", nil
	}
	return key, nil
}
