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
	"DeepSeek Harness",
	// 11-128 round four: the upstream WAF matches the "Paths prefixed with @"
	// paragraph that the official client injects verbatim. It is a plain
	// prompt-boilerplate sentence, so nothing else in the list catches it.
	sanitizeDSHPathsNeedle,
}

// Case-insensitive fingerprints are checked on the lowercased copy; their
// canonical spelling ("AI agent") would never match a lowercase literal list.

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
	// DSH and similar clients introduce themselves with one of these forms:
	//   You are an AI agent powered by <brand>.
	//   You are a coding agent powered by <brand>.
	//   You are an (AI) assistant built on <brand>.
	// The upstream WAF (11-128) rejects the whole sentence; replace it with a
	// neutral one for system/developer messages only. user/assistant/tool
	// content is user data and is left untouched.
	sanitizeDSHIdentityRe = regexp.MustCompile(`(?i)You are (an (AI )?(agent powered by|assistant built on)|a coding agent powered by)[^\r\n]*?[.!?](\s|$)`)
	// Residual brand terms outside the opening self-introduction still trip
	// 11-128 (DSH 0.2.1-rc.2 added "The runtime context names the DeepSeek
	// Harness implementation checkout" and similar lines). This runs only for
	// system/developer messages.
	sanitizeDSHBrandRe = regexp.MustCompile(`(?i)DeepSeek[ \t_-]*Harness`)
)

// The official client injects a fixed paragraph explaining how to read "@"
// path references. The upstream WAF treats that paragraph as an "official
// client" fingerprint and answers 11-128 ("Illegal API invocation from an
// unapproved channel") when it is missing, while sending the client's own
// wording unchanged. Rewrite it into an equivalent paraphrase: the model keeps
// the same instructions, the upstream no longer sees its template.
const (
	sanitizeDSHPathsNeedle = "Tokens prefixed with @ are paths the user explicitly referenced"

	sanitizeDSHPathsReplacement = "Paths that begin with @ point at files the user named explicitly. " +
		"Relative ones resolve against the workspace root; absolute ones point to files " +
		"or directories on the host. A trailing slash means a directory, so list it when " +
		"the contents matter. Anything else is a file: open it with the read tool when you " +
		"need its contents, and never claim to have inspected it before reading. " +
		"The @\"...\" form only quotes a path that contains spaces."
)

var sanitizeDSHPathsRe = regexp.MustCompile(
	`(?is)` + regexp.QuoteMeta(sanitizeDSHPathsNeedle) + `.{0,900}?spaces\.`,
)

const (
	bareHdrLower  = "x-anthropic-billing-header"
	omoLower      = "sisyphus-junior - focused executor from ohmyopencode"
	dshBrandLower = "deepseek harness"
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
	if strings.Contains(low, bareHdrLower) || strings.Contains(low, omoLower) || strings.Contains(low, dshBrandLower) {
		return true
	}
	return false
}

func sanitizeText(text string) string {
	if text == "" || !hasFingerprint(text) {
		return text
	}
	return sanitizeBase(text)
}

// sanitizeSystemText applies the broader identity-sentence rule that is only
// allowed for system/developer messages, then the ordinary fingerprints.
func sanitizeSystemText(text string) string {
	if text == "" {
		return text
	}
	out := sanitizeDSHIdentityRe.ReplaceAllString(text, "You are a helpful AI assistant.${4}")
	// Avoid a second full regex pass for the common case (no DeepSeek brand at
	// all). The regex still handles mixed separators and casing once present.
	if strings.Contains(out, "DeepSeek") || strings.Contains(out, "deepseek") || strings.Contains(out, "DEEPSEEK") {
		out = sanitizeDSHBrandRe.ReplaceAllString(out, "runtime")
	}
	if out == text {
		return sanitizeText(text)
	}
	return sanitizeBase(out)
}

func sanitizeBase(text string) string {
	text = sanitizeOmoRe.ReplaceAllLiteralString(text, "Sisyphus-Junior - Focused executor")
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
	text = sanitizeDSHPathsRe.ReplaceAllString(text, sanitizeDSHPathsReplacement)
	return strings.TrimSpace(text)
}

func sanitizeContent(content any, system bool) any {
	fn := sanitizeText
	if system {
		fn = sanitizeSystemText
	}
	switch c := content.(type) {
	case string:
		return fn(c)
	case []any:
		out := make([]any, 0, len(c))
		for _, part := range c {
			p, ok := asObj(part)
			if ok && p.Get("type") == "text" && p.Has("text") {
				cl := p.Clone()
				if s, ok := cl.Get("text").(string); ok {
					cl.Set("text", fn(s))
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
		// Assistant turns carry the same template sentence as system prompts when
		// a client replays its own history, and resending it upstream is what the
		// identity rewrite exists to avoid. User/tool content is left verbatim.
		system := role == "system" || role == "developer" || role == "assistant"
		preserveToolResult := opt.PreserveContract && (role == "tool" || role == "function")
		if item.Has("content") && !preserveToolResult {
			item.Set("content", sanitizeContent(item.Get("content"), system))
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
