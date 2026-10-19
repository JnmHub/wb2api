package openai

import (
	"encoding/json"
	"time"
)

// UsageEvent describes one finished request (success or failure).
type UsageEvent struct {
	At        time.Time
	Model     string
	Account   string
	KeyID     string
	Stream    bool
	Outcome   string // completed | client_aborted | upstream_aborted | failed
	Status    int
	Elapsed   time.Duration
	TTFT      time.Duration // request start -> first upstream chunk
	Gen       time.Duration // first -> last upstream chunk
	HeldFor   time.Duration // time text was withheld by the leak filter
	Usage     json.RawMessage
	Estimated bool // usage was estimated locally (not reported by upstream)
	Chunks    int
	Effort    string // reasoning effort the request ran at ("" = unknown)
}
