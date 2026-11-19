package codearts

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"wb2api/internal/jsonx"
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
			"tools":[{"type":"function","function":{"name":"other"}},{"type":"function","function":{"name":%q,"parameters":{"type":"object"}}}],
			"tool_choice":{"type":"function","function":{"name":%q}}
		}`, long, long))
		before, _ := jsonx.Marshal(shaped)
		body, names, err := PrepareBody(shaped, "deepseek-v4.1-flash", "")
		if err != nil {
			t.Fatal(err)
		}
		assertChoiceMatchesDeclaration(t, body)
		wire := parseObj(t, body)
		tools, _ := wire.Arr("tools")
		if name := toolFunctionName(tools[0]); names.RestoreName(name) != long {
			t.Fatalf("selected tool = %q", name)
		}
		after, _ := jsonx.Marshal(shaped)
		if string(before) != string(after) {
			t.Fatalf("preparation changed the original request: %s", after)
		}
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
		before, _ := jsonx.Marshal(shaped)
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
		after, _ := jsonx.Marshal(shaped)
		if string(before) != string(after) {
			t.Fatalf("preparation changed the original history: %s", after)
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
	if wire.Choice != "required" {
		t.Fatalf("tool_choice %v must require the selected declaration %q", wire.Choice, declared)
	}
}

// Combinations the upstream answers with InferHub.001001005 are dropped before
// the request goes out; everything else is forwarded as the client sent it.
func TestPrepareBodyDropsRejectedParamCombos(t *testing.T) {
	shaped := newObj(t, `{"messages":[{"role":"user","content":"hi"}],
		"max_tokens":256,"max_completion_tokens":512,"n":1,"stream_options":{"include_usage":false},
		"temperature":0.7,"top_p":0.9,"response_format":{"type":"json_object"}}`)
	body, _, err := PrepareBody(shaped, "deepseek-v4.1-flash", "")
	if err != nil {
		t.Fatal(err)
	}
	got := parseObj(t, body)
	if got.Has("max_completion_tokens") {
		t.Fatal("two output budgets must not both be sent")
	}
	if got.Has("n") || !IncludeUsage(got) {
		t.Fatal("wire must omit n and collect usage for accounting")
	}
	if raw := got.Get("max_tokens"); raw == nil || fmt.Sprint(raw) != "256" {
		t.Fatalf("max_tokens = %v", raw)
	}
	// Supported fields survive untouched.
	if !got.Has("temperature") || !got.Has("response_format") {
		t.Fatal("supported fields were removed")
	}
}

func TestPrepareBodyRejectsUnsupportedChoices(t *testing.T) {
	for _, input := range []string{
		`{"messages":[],"n":2}`,
		`{"messages":[],"n":"1"}`,
		`{"messages":[],"tool_choice":"required"}`,
		`{"messages":[],"tool_choice":{"type":"function","function":{"name":"absent"}}}`,
	} {
		if _, _, err := PrepareBody(newObj(t, input), "deepseek-v4.1-flash", ""); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}
