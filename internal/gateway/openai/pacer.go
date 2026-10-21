package openai

import (
	"context"
	"time"

	"wb2api/internal/store/settings"
)

// streamPacer limits how fast generated tokens are forwarded to a streaming
// client. It never speeds anything up and never delays the first content chunk
// (TTFT is unaffected); later chunks wait until the average rate is at most
// `rate` tokens/second. The upstream still generates and bills at its own pace:
// this is output shaping, not upstream throttling.
type streamPacer struct {
	rate    float64
	started bool
	start   time.Time
	emitted float64
}

func newStreamPacer(rate float64) *streamPacer {
	if rate <= 0 {
		return nil
	}
	return &streamPacer{rate: rate}
}

// wait records n output tokens and sleeps until emitting them at the configured
// average rate. The first call starts the clock without sleeping.
func (p *streamPacer) wait(ctx context.Context, n int) error {
	if p == nil || n <= 0 {
		return nil
	}
	if !p.started {
		p.started = true
		p.start = time.Now()
		p.emitted = float64(n)
		return nil
	}
	p.emitted += float64(n)
	target := time.Duration(p.emitted / p.rate * float64(time.Second))
	delay := target - time.Since(p.start)
	if delay <= 0 {
		return nil
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// streamTPS resolves the effective cap: per-key cap > 0 wins, -1 explicitly
// disables, 0 follows the global setting (itself 0 = off).
func (h *Handler) streamTPS(key *settings.APIKey) float64 {
	if key != nil {
		if key.TPSLimit > 0 {
			return key.TPSLimit
		}
		if key.TPSLimit < 0 {
			return 0
		}
	}
	if h.Settings != nil {
		return h.Settings.OutputTPSLimit()
	}
	return 0
}

// estimateTextTokens is the same rough estimator used for usage fallback: one
// token per CJK ideograph and 3.6 characters per token otherwise (minimum 1
// for any non-empty text). It is intentionally approximate; the upstream only
// reports exact usage at the end of the stream.
func estimateTextTokens(s string) int {
	if s == "" {
		return 0
	}
	cjk, other := 0, 0
	for _, r := range s {
		if r >= 0x4e00 && r <= 0x9fff {
			cjk++
		} else {
			other++
		}
	}
	n := cjk + int(float64(other)/3.6)
	if n < 1 {
		n = 1
	}
	return n
}

// chunkOutputTokens counts visible/reasoning output in one upstream data chunk.
func chunkOutputTokens(data []byte) int {
	c, ok := parseChunk(data)
	if !ok {
		return 0
	}
	n := 0
	for i := range c.deltas {
		n += estimateTextTokens(c.content(i))
		n += estimateTextTokens(c.reasoning(i))
	}
	return n
}
