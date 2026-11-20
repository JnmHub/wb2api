package codearts

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"wb2api/internal/jsonx"
)

func TestPrepareBodyTrimsOnlyCompletedReasoningAtWireThreshold(t *testing.T) {
	const threshold = 10 << 20
	const model, chatID = "deepseek-v4.1-flash", "history-affinity"
	const fixture = `{
		"messages": [
			{"role":"user","content":"Read the first file"},
			{
				"role":"assistant",
				"reasoning_content":"x",
				"reasoning":"old alias",
				"content":[
					{"type":"text","text":"正在读取文件"},
					{"type":"thinking","thinking":"unsigned old thought"},
					{"type":"thinking","thinking":"signed old thought","signature":"fixture-signature"},
					{"type":"redacted_thinking","data":"fixture-redacted-data"},
					{"type":"text","text":"I will use the tool."}
				],
				"tool_calls":[{"id":"call_old","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"first.txt\"}"}}]
			},
			{"role":"tool","content":"first file contents","tool_call_id":"call_old"},
			{"role":"assistant","content":"preserve continuation","prefix":true,"reasoning_content":"prefix thought"},
			{"role":"user","content":"Now read the second file"},
			{
				"role":"assistant",
				"reasoning_content":"thought for an active tool call",
				"tool_calls":[{"id":"call_current","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"second.txt\"}"}}]
			},
			{"role":"tool","content":"second file contents","tool_call_id":"call_current"},
			{
				"role":"assistant",
				"reasoning":"current alias",
				"content":[
					{"type":"thinking","thinking":"active structured thought"},
					{"type":"tool_use","id":"call_block","name":"read_file","input":{"path":"third.txt"}}
				]
			},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_block","content":"third file contents"}]}
		],
		"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}]
	}`
	base, _, err := PrepareBody(newObj(t, fixture), model, chatID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		size    int
		hasUser bool
		trims   bool
	}{
		{name: "one byte below threshold", size: threshold - 1, hasUser: true},
		{name: "at threshold", size: threshold, hasUser: true, trims: true},
		{name: "large with no user turn", size: threshold},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shaped := newObj(t, fixture)
			messages, _ := shaped.Arr("messages")
			history := messages[1].(*jsonx.Obj)
			if !tc.hasUser {
				messages = append([]any(nil), messages[1:4]...)
				shaped.Set("messages", messages)
			}
			// Use single-byte JSON-safe text so each added byte grows the wire
			// body by exactly one byte, including the gateway's wire fields.
			fillerSize := tc.size - len(base) + 1
			if !tc.hasUser {
				fillerSize = threshold
			}
			history.Set("reasoning_content", strings.Repeat("x", fillerSize))

			expected := append([]any(nil), messages...)
			if tc.trims {
				oldAssistant := history.Clone()
				oldAssistant.Delete("reasoning_content")
				oldAssistant.Delete("reasoning")
				blocks, _ := oldAssistant.Arr("content")
				kept := append([]any{blocks[0]}, blocks[2:]...)
				oldAssistant.Set("content", kept)
				expected[1] = oldAssistant
			}

			before, err := jsonx.Marshal(shaped)
			if err != nil {
				t.Fatal(err)
			}
			body, _, err := PrepareBody(shaped, model, chatID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.trims && len(body) >= threshold {
				t.Errorf("trimmed body = %d bytes, want less than %d", len(body), threshold)
			}
			if tc.hasUser && !tc.trims && len(body) != tc.size {
				t.Errorf("untrimmed body = %d bytes, want %d", len(body), tc.size)
			}
			if !tc.hasUser && len(body) <= threshold {
				t.Errorf("body with no user turn = %d bytes, want more than %d", len(body), threshold)
			}
			wire := parseObj(t, body)
			actual, _ := wire.Arr("messages")
			if len(actual) != len(expected) {
				t.Fatalf("messages = %d, want %d", len(actual), len(expected))
			}
			for i, message := range actual {
				if !reflect.DeepEqual(message, expected[i]) {
					role, _ := objStr(expected[i], "role")
					t.Errorf("wire message %d (role %s) differs from expected history", i, role)
				}
			}
			if !reflect.DeepEqual(wire.Get("tools"), shaped.Get("tools")) {
				t.Error("tool declarations changed")
			}
			after, err := jsonx.Marshal(shaped)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Error("preparation changed the original request")
			}
		})
	}
}
