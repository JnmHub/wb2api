package usageview

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestWithOriginalPromptPreservesUsageFields(t *testing.T) {
	usage := json.RawMessage(`{"prompt_tokens":12,"completion_tokens":5,"total_tokens":17,` +
		`"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":2},` +
		`"prompt_cache_hit_tokens":4,"vendor_metrics":{"credits":9007199254740993}}`)
	want := map[string]json.RawMessage{}
	if err := json.Unmarshal(usage, &want); err != nil {
		t.Fatal(err)
	}
	want["prompt_tokens"] = json.RawMessage(`112`)
	want["total_tokens"] = json.RawMessage(`117`)
	want["input_tokens_estimated"] = json.RawMessage(`true`)

	var got map[string]json.RawMessage
	if err := json.Unmarshal(WithOriginalPrompt(usage, 100), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("usage = %s, want all original fields with estimated prompt and adjusted total", WithOriginalPrompt(usage, 100))
	}
}

func TestWithOriginalPromptPartialOrAbsentUsage(t *testing.T) {
	for _, tc := range []struct {
		name, usage, want string
		removed           int
	}{
		{"completion missing", `{"prompt_tokens":2,"total_tokens":5}`, `{"input_tokens_estimated":true,"prompt_tokens":12,"total_tokens":15}`, 10},
		{"total missing", `{"prompt_tokens":2,"completion_tokens":3}`, `{"completion_tokens":3,"input_tokens_estimated":true,"prompt_tokens":12,"total_tokens":15}`, 10},
		{"prompt only", `{"prompt_tokens":0}`, `{"input_tokens_estimated":true,"prompt_tokens":10}`, 10},
		{"omitted usage", "", "", 10},
		{"null usage", `null`, `null`, 10},
		{"empty usage", `{}`, `{}`, 10},
		{"missing prompt", `{"completion_tokens":3}`, `{"completion_tokens":3}`, 10},
		{"null prompt", `{"prompt_tokens":null}`, `{"prompt_tokens":null}`, 10},
		{"invalid usage", `[1]`, `[1]`, 10},
		{"overflow", `{"prompt_tokens":9223372036854775807}`, `{"prompt_tokens":9223372036854775807}`, 10},
		{"no trim", `{"prompt_tokens":2}`, `{"prompt_tokens":2}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := WithOriginalPrompt(json.RawMessage(tc.usage), tc.removed); !bytes.Equal(got, []byte(tc.want)) {
				t.Fatalf("usage = %s, want %s", got, tc.want)
			}
		})
	}
}
