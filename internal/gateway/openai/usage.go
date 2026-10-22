package openai

import (
	"encoding/json"
	"time"
)

// UsageEvent describes one finished request (success or failure).
type UsageEvent struct {
	At            time.Time
	Model         string
	Account       string
	KeyID         string
	Stream        bool
	Outcome       string // completed | client_aborted | upstream_aborted | failed
	Status        int
	Elapsed       time.Duration
	TTFT          time.Duration // legacy: request start -> first upstream chunk (unchanged)
	UpstreamTTFT  time.Duration // final actual attempt dispatch -> first semantic read, before local pacing
	QueueWait     time.Duration // admission + key wait only; excludes retries/backoff/upstream work
	AdmissionWait time.Duration // accumulated request admission waits
	KeyWait       time.Duration // accumulated key/concurrency waits
	ClientTTFT    time.Duration // start -> successful semantic Flush (stream) or complete JSON Flush; NOT network arrival
	CancelLatency time.Duration // observed cancellation -> relay finalization; zero when unavailable
	Gen           time.Duration // first -> last upstream chunk
	HeldFor       time.Duration // time text was withheld by the leak filter
	Usage         json.RawMessage
	Estimated     bool // usage was estimated locally (not reported by upstream)
	Chunks        int
	Effort        string // reasoning effort the request ran at ("" = unknown)
}
