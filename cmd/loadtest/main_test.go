package main

import "testing"

func TestChatSemanticDelta(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want bool
	}{
		{"role", `{"choices":[{"delta":{"role":"assistant"}}]}`, false},
		{"empty", `{"choices":[{"delta":{"content":"","reasoning_content":"","tool_calls":[]}}]}`, false},
		{"null", `{"choices":[{"delta":{"content":null,"function_call":null}}]}`, false},
		{"content", `{"choices":[{"delta":{"content":"hi"}}]}`, true},
		{"reasoning", `{"choices":[{"delta":{"reasoning_content":"think"}}]}`, true},
		{"tool", `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"lookup"}}]}}]}`, true},
		{"function", `{"choices":[{"delta":{"function_call":{"name":"lookup"}}}]}`, true},
		{"usage", `{"choices":[],"usage":{"completion_tokens":2}}`, false},
		{"error", `{"error":{"message":"content delta failed"}}`, false},
		{"invalid", `not JSON`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := chatSemanticDelta([]byte(tc.data)); got != tc.want {
				t.Fatalf("semantic = %v, want %v", got, tc.want)
			}
		})
	}
}
