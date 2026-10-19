// Package usage extracts token and credit figures from an upstream usage block
// (port of legacy _extract_usage / _best_cached_tokens).
package usage

import (
	"bytes"
	"encoding/json"
	"math"
)

// Tokens are the figures the gateway tracks per request.
type Tokens struct {
	Prompt     int64
	Completion int64
	Reasoning  int64
	Cached     int64
	Total      int64
	Credit     float64
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func number(v any) float64 {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		if err != nil || math.IsNaN(f) {
			return 0
		}
		return f
	case float64:
		return t
	case bool:
		if t {
			return 1
		}
	}
	return 0
}

// BestCached picks the best cache-hit value across every alias upstreams emit:
// prompt_tokens_details.cached_tokens > prompt_cache_hit_tokens >
// cache_read_input_tokens > cached_tokens > input_tokens_details.cached_tokens >
// completion_tokens_details.cached_tokens. The first positive value wins, zero
// aliases never shadow a real hit.
func BestCached(u map[string]any) int64 {
	if u == nil {
		return 0
	}
	for _, v := range []any{
		asMap(u["prompt_tokens_details"])["cached_tokens"],
		u["prompt_cache_hit_tokens"],
		u["cache_read_input_tokens"],
		u["cached_tokens"],
		asMap(u["input_tokens_details"])["cached_tokens"],
		asMap(u["completion_tokens_details"])["cached_tokens"],
	} {
		if n := number(v); n > 0 {
			return int64(n)
		}
	}
	return 0
}

// Extract parses a usage block. ok is false when raw holds no usable figures.
func Extract(raw []byte) (t Tokens, ok bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return t, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var u map[string]any
	if err := dec.Decode(&u); err != nil || len(u) == 0 {
		return t, false
	}
	t.Prompt = int64(number(u["prompt_tokens"]))
	t.Completion = int64(number(u["completion_tokens"]))
	t.Reasoning = int64(number(asMap(u["completion_tokens_details"])["reasoning_tokens"]))
	t.Cached = BestCached(u)
	t.Total = int64(number(u["total_tokens"]))
	t.Credit = number(u["credit"])
	return t, true
}
