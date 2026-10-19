// Package leak recovers tool calls that the upstream model printed as text
// instead of emitting as structured tool_calls. It is a port of the DSML /
// "leaked call" machinery in legacy wb_proxy.py (parse_dsml_tool_calls,
// parse_leaked_tool_calls, LeakedToolCallStreamFilter).
//
// Parsing is behaviour-identical to the legacy code (verified against golden
// vectors generated from it). The streaming filter additionally offers a
// "precise" hold policy that stops withholding ordinary prose that merely
// resembles a call tag; see Filter.
package leak

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// Call is one recovered tool call.
type Call struct {
	ID        string
	Name      string
	Arguments string // JSON object text
}

func newCallID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "call_" + hex.EncodeToString(b[:])
}

var (
	// ｜ is the full-width vertical bar the model sometimes emits.
	dsmlBarsRe = regexp.MustCompile(`\x{ff5c}+\s*DSML\s*\x{ff5c}+`)

	leakParamRe = regexp.MustCompile(`(?is)<\s*(?:\|\s*DSML\s*\|\s*)?(?P<tag>parameter|param|arg|argument)\b` +
		`(?P<attrs>[^>]*)>` +
		`(?P<value>.*?)` +
		`</\s*(?:\|\s*DSML\s*\|\s*)?(?:parameter|param|arg|argument)\s*>`)
	leakTagPattern    = `<\s*/?\s*(?:\|\s*DSML\s*\|\s*)?(?P<name>[A-Za-z_][A-Za-z0-9_]*)(?P<attrs>[^>]*)>`
	leakTagRe         = regexp.MustCompile(leakTagPattern)
	leakTagAnchor     = regexp.MustCompile(`^` + leakTagPattern)
	leakTagAnchorFold = regexp.MustCompile(`(?i)^` + leakTagPattern)
	leakNameAttrRe    = regexp.MustCompile(`name\s*=\s*["']([^"']*)["']`)

	tagStart       = `<[^>]*DSML[^>]*`
	dsmlCallsRe    = regexp.MustCompile(`(?s)` + tagStart + `calls>(.*?)</[^>]*DSML[^>]*calls>`)
	dsmlInvokeRe   = regexp.MustCompile(`(?s)` + tagStart + `invoke\s+name=["']([^"']+)["']>(.*?)</[^>]*invoke>`)
	dsmlParamRe    = regexp.MustCompile(`(?s)` + tagStart + `parameter\s+name=["']([^"']+)["'][^>]*>(.*?)</[^>]*parameter>`)
	wrapperTags    = map[string]bool{"calls": true, "tool_calls": true, "function_calls": true, "function_call": true}
	paramTags      = map[string]bool{"parameter": true, "param": true, "arg": true, "argument": true}
	leakNameIdx    = leakTagRe.SubexpIndex("name")
	leakAttrsIdx   = leakTagRe.SubexpIndex("attrs")
	leakParamAttrs = leakParamRe.SubexpIndex("attrs")
	leakParamValue = leakParamRe.SubexpIndex("value")
)

// NormalizeDSMLBars folds the full-width spelling of the DSML token to ASCII.
func NormalizeDSMLBars(s string) string {
	if !strings.Contains(s, "｜") {
		return s
	}
	return dsmlBarsRe.ReplaceAllString(s, "|DSML|")
}

// orderedParams is a dict that remembers insertion order (Python semantics:
// re-assigning an existing key keeps its original position).
type orderedParams struct {
	keys []string
	vals map[string]string
}

func newOrdered() *orderedParams { return &orderedParams{vals: map[string]string{}} }

func (o *orderedParams) set(k, v string) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

// JSON renders the object like Python's json.dumps(ensure_ascii=False).
func (o *orderedParams) JSON() string {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(jsonString(k))
		b.WriteString(": ")
		b.WriteString(jsonString(o.vals[k]))
	}
	b.WriteByte('}')
	return b.String()
}

func jsonString(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	out := strings.TrimRight(b.String(), "\n")
	// Go always escapes these two; Python (ensure_ascii=False) does not.
	out = strings.ReplaceAll(out, ` `, " ")
	out = strings.ReplaceAll(out, ` `, " ")
	return out
}

// ParseDSML recovers calls from the full-width DSML spelling. ok is false when
// nothing was recognised (text is then returned unchanged).
func ParseDSML(text string) (calls []Call, clean string, ok bool) {
	if text == "" || !strings.Contains(text, "DSML") {
		return nil, text, false
	}
	loc := dsmlCallsRe.FindStringSubmatchIndex(text)
	if loc == nil {
		return nil, text, false
	}
	block := text[loc[2]:loc[3]]
	calls = []Call{}
	for _, im := range dsmlInvokeRe.FindAllStringSubmatch(block, -1) {
		params := newOrdered()
		for _, pm := range dsmlParamRe.FindAllStringSubmatch(im[2], -1) {
			params.set(pm[1], strings.TrimSpace(pm[2]))
		}
		calls = append(calls, Call{ID: newCallID(), Name: im[1], Arguments: params.JSON()})
	}
	clean = strings.TrimSpace(strings.TrimSpace(text[:loc[0]]) + " " + strings.TrimSpace(text[loc[1]:]))
	return calls, clean, true
}

type span struct{ start, end int }

func stripSpans(text string, spans []span) string {
	if len(spans) == 0 {
		return text
	}
	sorted := append([]span(nil), spans...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].start != sorted[j].start {
			return sorted[i].start < sorted[j].start
		}
		return sorted[i].end < sorted[j].end
	})
	var kept strings.Builder
	last := 0
	for _, s := range sorted {
		if s.start < last {
			continue
		}
		kept.WriteString(text[last:s.start])
		last = s.end
	}
	kept.WriteString(text[last:])
	return strings.TrimSpace(kept.String())
}

type foundParam struct {
	start int
	name  string
	value string
	sp    span
}

type pendingCall struct {
	name  string
	par   *orderedParams
	open  int // -1 = no owner tag
	spans []span
}

// ParseLeaked recovers a call the model printed instead of emitting. ok is
// false when nothing was recognised (original text returned).
func ParseLeaked(text string) (calls []Call, clean string, ok bool) {
	if text == "" || !strings.Contains(text, "<") {
		return nil, text, false
	}
	original := text
	text = NormalizeDSMLBars(text)

	var found []foundParam
	for _, m := range leakParamRe.FindAllStringSubmatchIndex(text, -1) {
		attrs := text[m[leakParamAttrs*2]:m[leakParamAttrs*2+1]]
		am := leakNameAttrRe.FindStringSubmatch(attrs)
		if am == nil || am[1] == "" {
			continue
		}
		val := strings.TrimSpace(text[m[leakParamValue*2]:m[leakParamValue*2+1]])
		found = append(found, foundParam{start: m[0], name: am[1], value: val, sp: span{m[0], m[1]}})
	}
	if len(found) == 0 {
		return nil, original, false
	}
	tags := leakTagRe.FindAllStringSubmatchIndex(text, -1)
	var pend []*pendingCall
	for _, f := range found {
		ownerIdx := -1
		for ti, tg := range tags {
			if tg[0] >= f.start {
				break
			}
			if strings.HasPrefix(strings.TrimLeft(text[tg[0]:tg[1]], " \t\r\n\v\f"), "</") {
				continue
			}
			tname := strings.ToLower(text[tg[leakNameIdx*2]:tg[leakNameIdx*2+1]])
			if wrapperTags[tname] || paramTags[tname] {
				continue
			}
			ownerIdx = ti
		}
		name := ""
		key := -1
		if ownerIdx >= 0 {
			tg := tags[ownerIdx]
			attrs := text[tg[leakAttrsIdx*2]:tg[leakAttrsIdx*2+1]]
			if am := leakNameAttrRe.FindStringSubmatch(attrs); am != nil {
				name = strings.TrimSpace(am[1])
			} else {
				name = strings.TrimSpace(text[tg[leakNameIdx*2]:tg[leakNameIdx*2+1]])
			}
			key = tg[0]
		}
		if name == "" {
			continue
		}
		if n := len(pend); n > 0 && pend[n-1].open == key {
			pend[n-1].par.set(f.name, f.value)
			pend[n-1].spans = append(pend[n-1].spans, f.sp)
		} else {
			p := newOrdered()
			p.set(f.name, f.value)
			pend = append(pend, &pendingCall{name: name, par: p, open: key, spans: []span{f.sp}})
		}
	}
	if len(pend) == 0 {
		return nil, original, false
	}

	var regions []span
	for _, c := range pend {
		first := c.open
		if first < 0 {
			first = c.spans[0].start
			for _, s := range c.spans {
				if s.start < first {
					first = s.start
				}
			}
		}
		last := 0
		for _, s := range c.spans {
			if s.end > last {
				last = s.end
			}
		}
		for {
			nxt := last
			for nxt < len(text) && strings.IndexByte(" \t\r\n", text[nxt]) >= 0 {
				nxt++
			}
			m := leakTagAnchor.FindStringIndex(text[nxt:])
			if m == nil || !strings.HasPrefix(strings.TrimLeft(text[nxt:nxt+m[1]], " \t\r\n\v\f"), "</") {
				break
			}
			last = nxt + m[1]
		}
		regions = append(regions, span{first, last})
	}
	clean = stripSpans(text, regions)

	var leftover []span
	for _, m := range leakTagRe.FindAllStringSubmatchIndex(clean, -1) {
		whole := clean[m[0]:m[1]]
		if strings.Contains(strings.ToUpper(NormalizeDSMLBars(whole)), "DSML") {
			leftover = append(leftover, span{m[0], m[1]})
		} else if wrapperTags[strings.ToLower(clean[m[leakNameIdx*2]:m[leakNameIdx*2+1]])] {
			leftover = append(leftover, span{m[0], m[1]})
		}
	}
	clean = stripSpans(clean, leftover)

	calls = make([]Call, 0, len(pend))
	for _, c := range pend {
		calls = append(calls, Call{ID: newCallID(), Name: c.name, Arguments: c.par.JSON()})
	}
	return calls, clean, true
}

// RepairText tries ParseLeaked, then ParseDSML, like repair_leaked_tool_calls
// does on a whole message body. ok reports whether calls were recovered.
func RepairText(content string) (calls []Call, clean string, ok bool) {
	if !strings.Contains(content, "<") {
		return nil, content, false
	}
	if c, cl, ok := ParseLeaked(content); ok && len(c) > 0 {
		return c, cl, true
	}
	if c, cl, ok := ParseDSML(content); ok && len(c) > 0 {
		return c, cl, true
	}
	return nil, content, false
}

// FindDSMLBlock locates the first complete DSML calls block in text (the
// legacy DSML_CALLS_RE.search): the byte range [start, end) of the whole block.
func FindDSMLBlock(text string) (start, end int, ok bool) {
	loc := dsmlCallsRe.FindStringIndex(text)
	if loc == nil {
		return 0, 0, false
	}
	return loc[0], loc[1], true
}
