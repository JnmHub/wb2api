package leak

import (
	"strings"
)

// ActionKind tells what a filter action carries.
type ActionKind int

const (
	// Text is assistant prose that may be forwarded to the client.
	Text ActionKind = iota
	// Calls are tool calls recovered from printed markup.
	Calls
)

// Action is one unit of filter output: either text to forward or recovered calls.
type Action struct {
	Kind  ActionKind
	Text  string
	Calls []Call
}

// Mode selects which text the stream filter withholds.
type Mode int

const (
	// Precise withholds text only while it can still turn into a printed tool
	// call: an exact call-tag name, or an incomplete tag that could become one.
	// Prose that merely starts like a call tag (<args>, <params>, <think>,
	// <thinking>, ...) is forwarded immediately. This is the default.
	Precise Mode = iota
	// Legacy reproduces the original behaviour exactly: any tag whose text
	// starts with a call-tag prefix (including <think...>) is withheld until the
	// stream ends. Kept for differential testing against the Python gateway.
	Legacy
)

// DefaultMaxHold is the legacy ceiling on withheld bytes (256 KiB).
const DefaultMaxHold = 262144

var legacyPrefixes = []string{
	"<run_code", "<run_shell", "<invoke", "<parameter", "<param", "<arg",
	"<argument", "<calls", "<tool_calls", "<function_calls", "<function_call",
	"<think", "<thinking",
	"</run_code", "</run_shell", "</invoke", "</parameter", "</param",
	"</arg", "</argument", "</calls", "</tool_calls", "</function_calls",
	"</function_call", "</think", "</thinking",
	"<|dsml|", "</|dsml|",
}

// precisePrefixes is legacyPrefixes minus the <think> family, which can never
// start a tool call.
var precisePrefixes = []string{
	"<run_code", "<run_shell", "<invoke", "<parameter", "<param", "<arg",
	"<argument", "<calls", "<tool_calls", "<function_calls", "<function_call",
	"</run_code", "</run_shell", "</invoke", "</parameter", "</param",
	"</arg", "</argument", "</calls", "</tool_calls", "</function_calls",
	"</function_call",
	"<|dsml|", "</|dsml|",
}

// callTagNames are the complete tag names that can belong to a printed call.
var callTagNames = map[string]bool{
	"run_code": true, "run_shell": true, "invoke": true, "parameter": true,
	"param": true, "arg": true, "argument": true, "calls": true,
	"tool_calls": true, "function_calls": true, "function_call": true,
}

// maxProbe bounds how much of a candidate tail is examined when no closing '>'
// has arrived yet. Beyond the legacy 96-character rule nothing can hold, so
// this only protects against pathological inputs.
const maxProbe = 4096

// Filter withholds assistant text that may be a printed tool call. It is not
// safe for concurrent use; one Filter serves one stream.
type Filter struct {
	// MaxHold is the byte ceiling for withheld text (0 = DefaultMaxHold).
	MaxHold int
	// Mode is the hold policy (zero value = Precise).
	Mode Mode

	buf     []byte
	decided bool // buf starts with a complete candidate tag: stays held
}

// NewFilter returns a Filter with the given mode and the default byte ceiling.
func NewFilter(mode Mode) *Filter { return &Filter{Mode: mode} }

// Held returns the number of bytes currently withheld.
func (f *Filter) Held() int { return len(f.buf) }

func (f *Filter) maxHold() int {
	if f.MaxHold > 0 {
		return f.MaxHold
	}
	return DefaultMaxHold
}

func (f *Filter) prefixes() []string {
	if f.Mode == Legacy {
		return legacyPrefixes
	}
	return precisePrefixes
}

// could mirrors could_be_leaked_call. complete reports that the decision rests
// on a finished tag (so it cannot change as more text arrives).
func (f *Filter) could(tail string) (hold, complete bool) {
	gt := strings.IndexByte(tail, '>')
	var window string
	switch {
	case gt >= 0:
		window = tail[:gt+1]
	case len(tail) > maxProbe:
		return false, false
	default:
		window = tail
	}
	if f.Mode != Legacy && gt < 0 && len(window) <= 200 &&
		(strings.HasPrefix(window, "<｜") || strings.HasPrefix(window, "</｜")) {
		// A full-width bar right after "<" is the start of a DSML tag that has not
		// finished arriving; it cannot be ordinary prose.
		return true, false
	}
	low := strings.ToLower(NormalizeDSMLBars(window))
	anchor := leakTagAnchor
	if f.Mode != Legacy {
		// The legacy regex is case-sensitive but runs on lower-cased text, so it
		// never recognises a finished <||DSML|| ...> tag. Precise mode folds case
		// so the DSML spelling is withheld and recovered like the plain one.
		anchor = leakTagAnchorFold
	}
	if loc := anchor.FindStringSubmatchIndex(low); loc != nil {
		tag := low[loc[0]:loc[1]]
		if f.Mode == Legacy {
			for _, p := range legacyPrefixes {
				if strings.HasPrefix(tag, p) {
					return true, true
				}
			}
		} else {
			name := low[loc[leakNameIdx*2]:loc[leakNameIdx*2+1]]
			if callTagNames[name] || strings.HasPrefix(tag, "<|dsml|") || strings.HasPrefix(tag, "</|dsml|") {
				return true, true
			}
		}
	}
	if len(low) > 96 || strings.Contains(low, ">") {
		return false, false
	}
	for _, p := range f.prefixes() {
		if strings.HasPrefix(p, low) || strings.HasPrefix(low, p) {
			return true, false
		}
	}
	return false, false
}

func (f *Filter) releaseAll() []Action {
	out := string(f.buf)
	f.buf = f.buf[:0]
	f.decided = false
	if out == "" {
		return nil
	}
	return []Action{{Kind: Text, Text: out}}
}

// Feed adds streamed text and returns what may be forwarded now.
func (f *Filter) Feed(text string) []Action {
	if text == "" {
		return nil
	}
	f.buf = append(f.buf, text...)
	if f.decided {
		if len(f.buf) > f.maxHold() {
			return f.releaseAll()
		}
		return nil
	}
	// Undecided buffers are small (one chunk plus a partial tag), so a string
	// copy here is cheap; the long-held path above never copies.
	s := string(f.buf)
	pos := 0
	for {
		rel := strings.IndexByte(s[pos:], '<')
		if rel < 0 {
			return f.releaseAll()
		}
		idx := pos + rel
		hold, complete := f.could(s[idx:])
		if !hold {
			pos = idx + 1
			continue
		}
		if idx > 0 {
			f.buf = append(f.buf[:0], s[idx:]...)
			f.decided = complete
			return []Action{{Kind: Text, Text: s[:idx]}}
		}
		f.decided = complete
		break
	}
	if len(f.buf) > f.maxHold() {
		return f.releaseAll()
	}
	return nil
}

// ReleaseHeld forces the withheld text out as prose (used by callers that
// enforce a wall-clock limit on holding).
func (f *Filter) ReleaseHeld() []Action { return f.releaseAll() }

// Flush is called at end of stream: whatever is still held is parsed for a
// printed call, otherwise released as text.
func (f *Filter) Flush() []Action {
	if len(f.buf) == 0 {
		return nil
	}
	buf := string(f.buf)
	f.buf = f.buf[:0]
	f.decided = false
	calls, clean, ok := RepairText(buf)
	if !ok {
		return []Action{{Kind: Text, Text: buf}}
	}
	var out []Action
	if clean != "" {
		out = append(out, Action{Kind: Text, Text: clean})
	}
	return append(out, Action{Kind: Calls, Calls: calls})
}
