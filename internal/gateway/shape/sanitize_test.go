package shape

import (
	"strings"
	"testing"

	"wb2api/internal/jsonx"
)

func TestDSHAgentFingerprintRewrite(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "harness agent powered by",
			in:   "You are an AI agent powered by DeepSeek Harness.",
			want: "You are a helpful AI assistant.",
		},
		{
			name: "coding agent powered by",
			in:   "You are a coding agent powered by the deepseek-v4.1-flash model.",
			want: "You are a helpful AI assistant.",
		},
		{
			name: "lowercase agent powered by",
			in:   "you are an ai agent powered by deepseek harness.",
			want: "You are a helpful AI assistant.",
		},
		{
			name: "residual implementation checkout brand",
			in:   "The runtime context names the DeepSeek Harness implementation checkout.",
			want: "The runtime context names the runtime implementation checkout.",
		},
		{
			name: "residual web gui brand",
			in:   "You are interacting with the user through the DeepSeek Harness Web GUI.",
			want: "You are interacting with the user through the runtime Web GUI.",
		},
		{
			name: "residual brand separators",
			in:   "The DeepSeek-Harness and DeepSeek_Harness runtime.",
			want: "The runtime and runtime runtime.",
		},
		{
			name: "harness",
			in:   "You are an AI assistant built on DeepSeek Harness.",
			want: "You are a helpful AI assistant.",
		},
		{
			name: "brand only",
			in:   "You are an AI assistant built on DeepSeek.",
			want: "You are a helpful AI assistant.",
		},
		{
			name: "coding model with dots",
			in:   "You are an assistant built on the deepseek-v4.1-flash model.",
			want: "You are a helpful AI assistant.",
		},
		{
			name: "lowercase",
			in:   "You are an AI assistant built on deepseek harness.",
			want: "You are a helpful AI assistant.",
		},
		{
			name: "keeps following text",
			in:   "You are an AI assistant built on X.\n\nUse the read tool before editing.",
			want: "You are a helpful AI assistant.\n\nUse the read tool before editing.",
		},
		{
			name: "no final period stays untouched",
			in:   "You are an AI assistant built on X",
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
			if got := sanitizeSystemText(c.in); got != c.want {
				t.Fatalf("sanitizeSystemText() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestDSHIdentityOnlyAppliesToSystemAndDeveloper(t *testing.T) {
	sentence := "You are an AI assistant built on DeepSeek Harness."
	msg := func(role, content string) any {
		o := jsonx.NewObj()
		o.Set("role", role)
		o.Set("content", content)
		return o
	}
	msgs := []any{msg("system", sentence), msg("developer", sentence), msg("user", sentence), msg("assistant", sentence)}
	out := sanitizeMessages(msgs, &Options{})
	for i, wantNeutral := range []bool{true, true, false, false} {
		item, _ := asObj(out[i])
		got, _ := item.Get("content").(string)
		if wantNeutral && got != "You are a helpful AI assistant." {
			t.Fatalf("message %d = %q, want neutral", i, got)
		}
		if !wantNeutral && got != sentence {
			t.Fatalf("message %d user data changed: %q", i, got)
		}
	}
}

func TestDSHSystemPromptKeepsToolContract(t *testing.T) {
	prompt := strings.Join([]string{
		"You are an AI assistant built on DeepSeek Harness.",
		"",
		"The runtime context names the DeepSeek Harness implementation checkout.",
		"You are interacting with the user through the DeepSeek Harness Web GUI.",
		"",
		"# Tools",
		"Use the read tool before editing a file.",
		"Never print secrets.",
	}, "\n")
	got := sanitizeSystemText(prompt)
	if !strings.HasPrefix(got, "You are a helpful AI assistant.") {
		t.Fatalf("opening attribution not neutralized: %q", got)
	}
	if strings.Contains(strings.ToLower(got), "deepseek harness") {
		t.Fatalf("residual brand survived: %q", got)
	}
	if !strings.Contains(got, "Use the read tool before editing a file.") || !strings.Contains(got, "Never print secrets.") {
		t.Fatalf("tool contract changed: %q", got)
	}
}

func TestDSHBrandOnlyAppliesToSystemAndDeveloper(t *testing.T) {
	sentence := "The runtime context names the DeepSeek Harness implementation checkout."
	msg := func(role, content string) any {
		o := jsonx.NewObj()
		o.Set("role", role)
		o.Set("content", content)
		return o
	}
	msgs := []any{msg("system", sentence), msg("developer", sentence), msg("user", sentence), msg("assistant", sentence)}
	out := sanitizeMessages(msgs, &Options{})
	for i, wantChanged := range []bool{true, true, false, false} {
		item, _ := asObj(out[i])
		got, _ := item.Get("content").(string)
		if wantChanged {
			if strings.Contains(strings.ToLower(got), "deepseek harness") {
				t.Fatalf("system/developer brand survived: %q", got)
			}
			if !strings.Contains(got, "runtime implementation checkout") {
				t.Fatalf("system/developer text changed unexpectedly: %q", got)
			}
		} else if got != sentence {
			t.Fatalf("user data changed: %q", got)
		}
	}
}

func TestDSHBrandIsAFingerprint(t *testing.T) {
	if !hasFingerprint("The DeepSeek Harness implementation checkout") {
		t.Fatal("brand-only system prompt must enter the sanitizer")
	}
	if got := sanitizeSystemText("The DeepSeek Harness implementation checkout"); hasFingerprint(got) {
		t.Fatalf("sanitized text still has a fingerprint: %q", got)
	}
}
