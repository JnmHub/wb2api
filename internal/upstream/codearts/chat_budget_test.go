package codearts

import (
	"encoding/json"
	"testing"

	"wb2api/internal/jsonx"
)

func TestPrepareBodyWithBudget(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		key, wire   string
		budget      int64
	}{
		{"absent", `{}`, "", "", 0},
		{"small", `{"max_tokens":16}`, "max_tokens", "16", 16},
		{"boundary", `{"max_tokens":65536}`, "max_tokens", "65536", 65536},
		{"above boundary", `{"max_tokens":65537}`, "max_tokens", "65536", 65536},
		{"catalogue default", `{"max_tokens":131072}`, "max_tokens", "65536", 65536},
		{"large integer", `{"max_tokens":18446744073709551616}`, "max_tokens", "65536", 65536},
		{"overflow", `{"max_tokens":1e999}`, "max_tokens", "65536", 65536},
		{"completion alias", `{"max_completion_tokens":128}`, "max_completion_tokens", "128", 128},
		{"completion alias above boundary", `{"max_completion_tokens":131072}`, "max_completion_tokens", "65536", 65536},
		{"prefer smaller primary", `{"max_tokens":16,"max_completion_tokens":131072}`, "max_tokens", "16", 16},
		{"cap preferred primary", `{"max_completion_tokens":16,"max_tokens":131072}`, "max_tokens", "65536", 65536},
		{"zero", `{"max_tokens":0}`, "max_tokens", "0", 0},
		{"negative", `{"max_tokens":-1}`, "max_tokens", "-1", 0},
		{"fractional", `{"max_tokens":1.5}`, "max_tokens", "1.5", 0},
		{"fractional above boundary", `{"max_tokens":65536.5}`, "max_tokens", "65536.5", 0},
		{"fraction below float precision", `{"max_tokens":65537.000000000000000001}`, "max_tokens", "65537.000000000000000001", 0},
		{"whole exponent", `{"max_tokens":10.0e-1}`, "max_tokens", "10.0e-1", 1},
		{"invalid string", `{"max_tokens":"invalid"}`, "max_tokens", `"invalid"`, 0},
		{"null primary", `{"max_tokens":null,"max_completion_tokens":16}`, "max_tokens", "null", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shaped := newObj(t, tc.input)
			body, _, budget, err := PrepareBodyWithBudget(shaped, "GLM-5.2", "")
			if err != nil {
				t.Fatal(err)
			}
			if budget != tc.budget {
				t.Errorf("effective budget = %d, want %d", budget, tc.budget)
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(body, &wire); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"max_tokens", "max_completion_tokens"} {
				want := ""
				if key == tc.key {
					want = tc.wire
				}
				if got := string(wire[key]); got != want {
					t.Errorf("wire %s = %q, want %q", key, got, want)
				}
			}
			original, err := jsonx.Marshal(shaped)
			if err != nil || string(original) != tc.input {
				t.Errorf("shaped request after prepare = %s, want %s (err %v)", original, tc.input, err)
			}
		})
	}
}
