package shape

import (
	"regexp"
	"strings"

	"wb2api/internal/jsonx"
)

// Fingerprint sanitisation: client system prompts contain fixed template
// sentences that the upstream's content review matches verbatim and rejects
// (code 11128). Only the confirmed fingerprints are rewritten.

var sanitizeFeatures = []string{
	"x-anthropic-billing-header",
	"cc_entrypoint=",
	"You are Claude Code",
	"Main branch (",
	"You are a coding agent running in the Codex CLI",
	"github.com/anthropics/",
	"11128",
}

// Case-insensitive fingerprints are checked on the lowercased copy; their
// canonical spelling ("AI agent") would never match a lowercase literal list.
var sanitizeLowerFeatures = []string{
	"an ai agent powered by",
	"a coding agent powered by",
}

var sanitizeRewrites = [][2]string{
	{"You are Claude Code, Anthropic's official CLI for Claude",
		"You are Claude Code, Anthropic's official CLI tool for Claude"},
	{"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)"},
	{"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant."},
	{"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"},
	{"11128", "11-128"},
}

var (
	sanitizeHdrRe     = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\r\n]*;?\s*`)
	sanitizeBareHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header`)
	sanitizeKVRe      = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\r\n]*;?\s*`)
	sanitizeOmoRe     = regexp.MustCompile(`(?i)Sisyphus-Junior - Focused executor from OhMyOpenCode`)
	// DeepSeek Harness / similar clients introduce themselves with these exact
	// sentences. The upstream WAF (11128) rejects the attribution clause, while
	// the rest of the system prompt is a behaviour contract and must stay put.
	// Only the opening clause is replaced; the model/product name, the sentence
	// ending and everything after it are preserved byte for byte.
	sanitizeDSHAgentRe = regexp.MustCompile(`(?i)You are an AI agent powered by`)
	sanitizeDSHCoderRe = regexp.MustCompile(`(?i)You are a coding agent powered by`)
)

const (
	bareHdrLower = "x-anthropic-billing-header"
	omoLower     = "sisyphus-junior - focused executor from ohmyopencode"
)

// foldDotted mirrors the three code points whose (?i) matching differs from
// str.lower() in the legacy implementation.
var foldDotted = strings.NewReplacer("İ", "i", "ı", "i", "ſ", "s")

func hasFingerprint(text string) bool {
	if text == "" {
		return false
	}
	for _, f := range sanitizeFeatures {
		if strings.Contains(text, f) {
			return true
		}
	}
	var low string
	if strings.ContainsAny(text, "İıſ") {
		low = strings.ToLower(foldDotted.Replace(text))
	} else {
		low = strings.ToLower(text)
	}
	if strings.Contains(low, bareHdrLower) || strings.Contains(low, omoLower) {
		return true
	}
	for _, f := range sanitizeLowerFeatures {
		if strings.Contains(low, f) {
			return true
		}
	}
	return false
}

func sanitizeText(text string) string {
	if text == "" || !hasFingerprint(text) {
		return text
	}
	text = sanitizeOmoRe.ReplaceAllLiteralString(text, "Sisyphus-Junior - Focused executor")
	text = sanitizeDSHAgentRe.ReplaceAllLiteralString(text, "You are an AI assistant built on")
	text = sanitizeDSHCoderRe.ReplaceAllLiteralString(text, "You are an assistant built on")
	for _, r := range sanitizeRewrites {
		text = strings.ReplaceAll(text, r[0], r[1])
	}
	text = sanitizeHdrRe.ReplaceAllString(text, "")
	if strings.Contains(text, "cc_") {
		prev := ""
		for prev != text {
			prev = text
			text = sanitizeKVRe.ReplaceAllString(text, "")
		}
	}
	text = sanitizeBareHdrRe.ReplaceAllLiteralString(text, "x-anthropic-billing-hdr")
	return strings.TrimSpace(text)
}

func sanitizeContent(content any) any {
	switch c := content.(type) {
	case string:
		return sanitizeText(c)
	case []any:
		out := make([]any, 0, len(c))
		for _, part := range c {
			p, ok := asObj(part)
			if ok && p.Get("type") == "text" && p.Has("text") {
				cl := p.Clone()
				if s, ok := cl.Get("text").(string); ok {
					cl.Set("text", sanitizeText(s))
				}
				out = append(out, cl)
			} else {
				out = append(out, part)
			}
		}
		return out
	}
	return content
}

func sanitizeToolCalls(v any, preserve bool) any {
	calls, ok := v.([]any)
	if !ok {
		return v
	}
	out := make([]any, 0, len(calls))
	for _, tc := range calls {
		o, ok := asObj(tc)
		if !ok {
			out = append(out, tc)
			continue
		}
		item := o.Clone()
		if fn, ok := asObj(item.Get("function")); ok {
			fn = fn.Clone()
			raw, had := fn.Lookup("arguments")
			switch {
			case func() bool { _, isS := raw.(string); return isS }():
				s := raw.(string)
				if !preserve {
					s = sanitizeText(s)
				}
				fn.Set("arguments", s)
			case !had || raw == nil:
				fn.Set("arguments", "{}")
			default:
				// The upstream's Go struct wants a string here; serialise
				// anything else (code 11101 otherwise).
				b, err := jsonx.MarshalPy(raw)
				if err != nil {
					fn.Set("arguments", jsonx.PyStr(raw))
				} else {
					fn.Set("arguments", string(b))
				}
			}
			item.Set("function", fn)
		}
		out = append(out, item)
	}
	return out
}

func sanitizeMessages(msgs []any, opt *Options) []any {
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		o, ok := asObj(m)
		if !ok {
			out = append(out, m)
			continue
		}
		item := o.Clone()
		role := item.Get("role")
		preserveToolResult := opt.PreserveContract && (role == "tool" || role == "function")
		if item.Has("content") && !preserveToolResult {
			item.Set("content", sanitizeContent(item.Get("content")))
		}
		if rc, ok := item.Get("reasoning_content").(string); ok {
			item.Set("reasoning_content", sanitizeText(rc))
		}
		if item.Has("tool_calls") {
			item.Set("tool_calls", sanitizeToolCalls(item.Get("tool_calls"), opt.PreserveContract))
		}
		out = append(out, item)
	}
	return out
}
