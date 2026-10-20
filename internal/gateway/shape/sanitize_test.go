package shape

import (
	"strings"
	"testing"
)

func TestDSHAgentFingerprintRewrite(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "harness",
			in:   "You are an AI agent powered by DeepSeek Harness.\n\nUse the read tool before editing.",
			want: "You are an AI assistant built on DeepSeek Harness.\n\nUse the read tool before editing.",
		},
		{
			name: "coding model with dots",
			in:   "You are a coding agent powered by the deepseek-v4.1-flash model.",
			want: "You are an assistant built on the deepseek-v4.1-flash model.",
		},
		{
			name: "lowercase",
			in:   "you are an ai agent powered by deepseek harness.",
			want: "You are an AI assistant built on deepseek harness.",
		},
		{
			name: "no final period",
			in:   "You are an AI agent powered by X",
			want: "You are an AI assistant built on X",
		},
		{
			name: "other attribution untouched",
			in:   "The AI agent powered by X is a tool.",
			want: "The AI agent powered by X is a tool.",
		},
		{
			name: "single words untouched",
			in:   "You are an AI agent. Powered by X.",
			want: "You are an AI agent. Powered by X.",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeText(c.in); got != c.want {
				t.Fatalf("sanitizeText() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestDSHFingerprintIsDetectedCaseInsensitively(t *testing.T) {
	for _, s := range []string{
		"You are an AI agent powered by DeepSeek Harness.",
		"you are an ai agent powered by DeepSeek Harness.",
		"You are a coding agent powered by the deepseek-v4.1-flash model.",
	} {
		if !hasFingerprint(s) {
			t.Fatalf("fingerprint not detected in %q", s)
		}
	}
	for _, s := range []string{
		"You are an AI agent.",
		"DeepSeek Harness is a product.",
		"You are a helpful assistant.",
	} {
		if hasFingerprint(s) {
			t.Fatalf("false positive for %q", s)
		}
	}
}

func TestDSHSystemPromptKeepsToolContract(t *testing.T) {
	// The sentences after the vendor line are the client's behaviour contract;
	// only the opening attribution may change.
	prompt := strings.Join([]string{
		"You are an AI agent powered by DeepSeek Harness.",
		"",
		"# Tools",
		"Use the read tool before editing a file.",
		"Never print secrets.",
	}, "\n")
	got := sanitizeText(prompt)
	if !strings.Contains(got, "You are an AI assistant built on DeepSeek Harness.") {
		t.Fatalf("opening attribution not rewritten: %q", got)
	}
	if !strings.Contains(got, "Use the read tool before editing a file.") || !strings.Contains(got, "Never print secrets.") {
		t.Fatalf("tool contract changed: %q", got)
	}
}
