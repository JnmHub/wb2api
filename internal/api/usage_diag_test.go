package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"wb2api/internal/gateway/openai"
)

// A failed row must keep the vendor code, not just "Bad Request": two 400s with
// different causes were indistinguishable before.
func TestEventFromGatewayKeepsUpstreamCode(t *testing.T) {
	ev := openai.UsageEvent{
		Model: "deepseek-v4.1-flash", Account: "a", Outcome: "failed", Status: http.StatusBadRequest,
		UpstreamCode: "InferHub.001001005.400", RequestID: "req-1",
		Shape: &openai.RequestShape{Bytes: 812, Messages: 3, Tools: 1, HasToolChoice: true, LongToolNames: 1},
	}
	row := EventFromGateway(ev, func(string) string { return "codearts" })
	if row.Error != "upstream InferHub.001001005.400" {
		t.Fatalf("error = %q", row.Error)
	}
	var extra struct {
		RequestID    string               `json:"request_id"`
		UpstreamCode string               `json:"upstream_code"`
		Shape        *openai.RequestShape `json:"request_shape"`
	}
	if err := json.Unmarshal(row.Extra, &extra); err != nil {
		t.Fatalf("extra: %v", err)
	}
	if extra.RequestID != "req-1" || extra.UpstreamCode != "InferHub.001001005.400" {
		t.Fatalf("extra = %+v", extra)
	}
	if extra.Shape == nil || extra.Shape.Bytes != 812 || extra.Shape.Tools != 1 {
		t.Fatalf("shape = %+v", extra.Shape)
	}

	// Without a code the old behaviour stands.
	plain := EventFromGateway(openai.UsageEvent{Outcome: "failed", Status: http.StatusBadRequest}, nil)
	if plain.Error != "Bad Request" {
		t.Fatalf("plain error = %q", plain.Error)
	}
}
