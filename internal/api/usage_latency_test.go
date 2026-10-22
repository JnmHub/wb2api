package api

import (
	"encoding/json"
	"testing"
	"time"

	"wb2api/internal/gateway/openai"
)

func TestEventFromGatewayAdditiveLatency(t *testing.T) {
	e := EventFromGateway(openai.UsageEvent{
		TTFT: time.Second, Gen: 2 * time.Second,
		UpstreamTTFT: 50 * time.Millisecond, ClientTTFT: 120 * time.Millisecond,
		QueueWait: 15 * time.Millisecond, AdmissionWait: 10 * time.Millisecond,
		KeyWait: 5 * time.Millisecond, CancelLatency: 500 * time.Microsecond,
	}, nil)
	if e.TTFTMs == nil || *e.TTFTMs != 1000 || e.GenMs == nil || *e.GenMs != 2000 {
		t.Fatalf("legacy columns changed: %+v", e)
	}
	got := map[string]any{}
	addLatencyExtra(got, e.Extra)
	want := map[string]float64{"upstream_ttft_ms": 50, "client_ttft_ms": 120, "queue_wait_ms": 15, "admission_wait_ms": 10, "key_wait_ms": 5, "cancel_latency_ms": 0.5}
	if len(got) != len(want) {
		t.Fatalf("extra=%s decoded=%v", e.Extra, got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s=%v want%v", k, got[k], v)
		}
	}
	missing := EventFromGateway(openai.UsageEvent{UpstreamTTFT: -time.Second}, nil)
	if len(missing.Extra) != 0 {
		t.Fatalf("absent/negative latency persisted: %s", missing.Extra)
	}
}

func TestLatencyExtraSafeAllowlist(t *testing.T) {
	for _, extra := range []string{
		`{"upstream_ttft_ms":3,"client_ttft_ms":-2,"queue_wait_ms":0,"error":"secret","ttft_ms":999,"account":"override"}`,
		`{"upstream_ttft_ms":"bad","client_ttft_ms":10}`,
		`not json`,
	} {
		got := map[string]any{"ttft_ms": float64(100), "account": "safe"}
		addLatencyExtra(got, json.RawMessage(extra))
		if got["ttft_ms"] != float64(100) || got["account"] != "safe" {
			t.Fatalf("legacy field overridden: %v", got)
		}
		if _, ok := got["error"]; ok {
			t.Fatalf("arbitrary extra exposed: %v", got)
		}
		if _, ok := got["client_ttft_ms"]; ok {
			t.Fatalf("invalid duration exposed: %v", got)
		}
		if _, ok := got["queue_wait_ms"]; ok {
			t.Fatalf("zero duration exposed: %v", got)
		}
	}
}

func TestUsageRecentAdditiveLatencyRoundTrip(t *testing.T) {
	now := time.Now()
	u, mux := newUsage(t, now)
	u.DB.Record(EventFromGateway(openai.UsageEvent{At: now, Model: "m", Outcome: "completed", Status: 200,
		TTFT: time.Second, UpstreamTTFT: 20 * time.Millisecond, ClientTTFT: 60 * time.Millisecond,
		QueueWait: 10 * time.Millisecond, KeyWait: 10 * time.Millisecond}, nil))
	u.DB.Flush()
	code, out := get(t, mux, "/api/v2/usage/recent?range=all")
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, out)
	}
	rows := out["requests"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows=%v", rows)
	}
	row := rows[0].(map[string]any)
	for key, want := range map[string]float64{"ttft_ms": 1000, "upstream_ttft_ms": 20, "client_ttft_ms": 60, "queue_wait_ms": 10, "key_wait_ms": 10} {
		if row[key] != want {
			t.Errorf("%s=%v want%v", key, row[key], want)
		}
	}
	if _, ok := row["cancel_latency_ms"]; ok {
		t.Fatalf("missing duration exposed: %v", row)
	}
}
