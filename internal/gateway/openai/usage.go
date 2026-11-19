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

	// UpstreamCode is the vendor's business code (InferHub.001001005.400,
	// TM.00001041, ...) when the failure came from upstream. Without it every
	// HTTP 400 reads as "Bad Request" in the ledger.
	UpstreamCode string
	// RequestID is what the client got back in X-Request-Id / error.request_id.
	RequestID string
	// Shape describes the outbound body structurally (no content) so a rejected
	// request can be rebuilt without the original text.
	Shape            *RequestShape
	CodeArtsAttempts []CodeArtsAttempt
}

// CodeArtsAttempt records the actual account and outbound phases without
// recording any request, response, or credential contents.
type CodeArtsAttempt struct {
	Account string             `json:"account"`
	Result  string             `json:"result,omitempty"`
	Phases  map[string]float64 `json:"phases_ms,omitempty"`
}

// RequestShape is a structural summary of what was actually sent upstream.
// It never contains message text, tool arguments, headers or credentials.
type RequestShape struct {
	Bytes         int    `json:"bytes"`
	Messages      int    `json:"messages"`
	Tools         int    `json:"tools,omitempty"`
	HasToolChoice bool   `json:"has_tool_choice,omitempty"`
	ToolChoice    string `json:"tool_choice_kind,omitempty"`
	Stream        bool   `json:"stream"`
	MaxTokens     int64  `json:"max_tokens,omitempty"`
	LongToolNames int    `json:"tool_names_over_64,omitempty"`
}
