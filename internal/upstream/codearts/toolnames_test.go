package codearts

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestShortenTools(t *testing.T) {
	long := strings.Repeat("very_long_tool_name_", 5) // ~100 chars
	tools := []any{
		map[string]any{"type": "function", "function": map[string]any{"name": long}},
		map[string]any{"type": "function", "function": map[string]any{"name": "short"}},
	}
	names := ShortenTools(tools)
	if names == nil || names.Empty() {
		t.Fatal("nothing was shortened")
	}
	sent := tools[0].(map[string]any)["function"].(map[string]any)["name"].(string)
	if len([]rune(sent)) > MaxToolNameChars {
		t.Fatalf("sent name is %d chars: %q", len([]rune(sent)), sent)
	}
	if !strings.HasPrefix(sent, "very_long_tool_name_") {
		t.Fatalf("sent name lost its shape: %q", sent)
	}
	if got := tools[1].(map[string]any)["function"].(map[string]any)["name"]; got != "short" {
		t.Fatalf("a short name was rewritten: %v", got)
	}
	if names.RestoreName(sent) != long {
		t.Fatalf("restore = %q", names.RestoreName(sent))
	}
	if names.RestoreName("short") != "short" {
		t.Fatal("unknown names pass through")
	}
}

// A generated short name must not collide with a name the client already uses.
func TestShortenToolsAvoidsCollisions(t *testing.T) {
	prefix := strings.Repeat("x", 80)
	a := prefix + "a"
	b := prefix + "b"
	tools := []any{
		map[string]any{"function": map[string]any{"name": a}},
		map[string]any{"function": map[string]any{"name": b}},
	}
	names := ShortenTools(tools)
	if names == nil {
		t.Fatal("nothing shortened")
	}
	na := tools[0].(map[string]any)["function"].(map[string]any)["name"].(string)
	nb := tools[1].(map[string]any)["function"].(map[string]any)["name"].(string)
	if na == nb {
		t.Fatalf("two tools collapsed onto %q", na)
	}
	if names.RestoreName(na) != a || names.RestoreName(nb) != b {
		t.Fatal("restore mapping is wrong")
	}
}

func TestRestoreJSON(t *testing.T) {
	long := strings.Repeat("mcp_server_tool_", 6)
	tools := []any{map[string]any{"function": map[string]any{"name": long}}}
	names := ShortenTools(tools)
	sent := tools[0].(map[string]any)["function"].(map[string]any)["name"].(string)

	chunk := []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"` + sent + `","arguments":"{}"}}]}}]}`)
	out, changed := names.RestoreJSON(chunk)
	if !changed {
		t.Fatal("the chunk was not restored")
	}
	var doc struct {
		Choices []struct {
			Delta struct {
				ToolCalls []struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc.Choices[0].Delta.ToolCalls[0].Function.Name; got != long {
		t.Fatalf("restored name = %q", got)
	}

	// A chunk without the short name keeps its exact bytes.
	plain := []byte(`{"choices":[{"delta":{"content":"hi"}}]}`)
	out2, changed := names.RestoreJSON(plain)
	if changed || string(out2) != string(plain) {
		t.Fatal("untouched chunks must pass through byte for byte")
	}
}

// The non-stream reply carries the restored name too.
func TestRestoreMessageShape(t *testing.T) {
	long := strings.Repeat("a_very_long_mcp_tool_", 5)
	tools := []any{map[string]any{"function": map[string]any{"name": long}}}
	names := ShortenTools(tools)
	sent := tools[0].(map[string]any)["function"].(map[string]any)["name"].(string)
	chunk := []byte(`{"choices":[{"message":{"tool_calls":[{"function":{"name":"` + sent + `","arguments":"{}"}}]}}]}`)
	out, changed := names.RestoreJSON(chunk)
	if !changed || !strings.Contains(string(out), long) {
		t.Fatalf("message shape not restored: %s", out)
	}
}
