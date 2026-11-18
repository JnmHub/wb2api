package codearts

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// A shortened declaration must be referenced by the same name everywhere else:
// tool_choice and earlier tool calls otherwise point at a tool the request no
// longer contains, and the upstream answers InferHub.001001005.
func TestShortenedToolNamesStayConsistent(t *testing.T) {
	long := strings.Repeat("a", 65)

	t.Run("explicit string choice", func(t *testing.T) {
		shaped := newObj(t, fmt.Sprintf(`{
			"messages":[{"role":"user","content":"hi"}],
			"tools":[{"type":"function","function":{"name":%q,"parameters":{"type":"object"}}}],
			"tool_choice":%q
		}`, long, long))
		body, names, err := PrepareBody(shaped, "deepseek-v4.1-flash", "")
		if err != nil {
			t.Fatal(err)
		}
		if names == nil {
			t.Fatal("nothing was shortened")
		}
		assertChoiceMatchesDeclaration(t, body)
	})

	t.Run("object choice", func(t *testing.T) {
		shaped := newObj(t, fmt.Sprintf(`{
			"messages":[{"role":"user","content":"hi"}],
			"tools":[{"type":"function","function":{"name":%q,"parameters":{"type":"object"}}}],
			"tool_choice":{"type":"function","function":{"name":%q}}
		}`, long, long))
		body, _, err := PrepareBody(shaped, "deepseek-v4.1-flash", "")
		if err != nil {
			t.Fatal(err)
		}
		assertChoiceMatchesDeclaration(t, body)
	})

	t.Run("history tool_calls", func(t *testing.T) {
		shaped := newObj(t, fmt.Sprintf(`{
			"messages":[
				{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":%q,"arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"c1","content":"ok"},
				{"role":"user","content":"again"}
			],
			"tools":[{"type":"function","function":{"name":%q,"parameters":{"type":"object"}}}]
		}`, long, long))
		body, _, err := PrepareBody(shaped, "deepseek-v4.1-flash", "")
		if err != nil {
			t.Fatal(err)
		}
		var wire struct {
			Tools    []struct{ Function struct{ Name string } } `json:"tools"`
			Messages []struct {
				ToolCalls []struct {
					Function struct{ Name string } `json:"function"`
				} `json:"tool_calls"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &wire); err != nil {
			t.Fatal(err)
		}
		declared := wire.Tools[0].Function.Name
		if len([]rune(declared)) > MaxToolNameChars {
			t.Fatalf("declaration is still %d chars", len([]rune(declared)))
		}
		if got := wire.Messages[0].ToolCalls[0].Function.Name; got != declared {
			t.Fatalf("history name %q does not match the declaration %q", got, declared)
		}
	})
}

func assertChoiceMatchesDeclaration(t *testing.T, body []byte) {
	t.Helper()
	var wire struct {
		Tools  []struct{ Function struct{ Name string } } `json:"tools"`
		Choice any                                        `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Tools) != 1 {
		t.Fatalf("tools = %d", len(wire.Tools))
	}
	declared := wire.Tools[0].Function.Name
	if len([]rune(declared)) > MaxToolNameChars {
		t.Fatalf("declaration is still %d chars", len([]rune(declared)))
	}
	var chosen string
	switch c := wire.Choice.(type) {
	case string:
		chosen = c
	case map[string]any:
		if fn, ok := c["function"].(map[string]any); ok {
			chosen, _ = fn["name"].(string)
		}
	}
	if chosen != declared {
		t.Fatalf("tool_choice %q does not match the declared %q", chosen, declared)
	}
}

// Combinations the upstream answers with InferHub.001001005 are dropped before
// the request goes out; everything else is forwarded as the client sent it.
func TestPrepareBodyDropsRejectedParamCombos(t *testing.T) {
	shaped := newObj(t, `{"messages":[{"role":"user","content":"hi"}],
		"max_tokens":256,"max_completion_tokens":512,"n":3,"stream_options":{"include_usage":true},
		"temperature":0.7,"top_p":0.9,"response_format":{"type":"json_object"}}`)
	body, _, err := PrepareBody(shaped, "deepseek-v4.1-flash", "")
	if err != nil {
		t.Fatal(err)
	}
	got := parseObj(t, body)
	if got.Has("max_completion_tokens") {
		t.Fatal("two output budgets must not both be sent")
	}
	if got.Has("n") || got.Has("stream_options") {
		t.Fatal("unsupported fields must be dropped")
	}
	if raw := got.Get("max_tokens"); raw == nil || fmt.Sprint(raw) != "256" {
		t.Fatalf("max_tokens = %v", raw)
	}
	// Supported fields survive untouched.
	if !got.Has("temperature") || !got.Has("response_format") {
		t.Fatal("supported fields were removed")
	}
}
