package openai

import (
	"encoding/json"
	"strings"
	"unicode"
)

const (
	assistantOpen  = "<assistant_response>"
	assistantClose = "</assistant_response>"
	thinkingOpen   = "<thinking>"
	thinkingClose  = "</thinking>"
	thinkOpen      = "<think>"
	thinkClose     = "</think>"
	maxTextPrelude = 128
)

type codeArtsTextPhase uint8

const (
	codeArtsTextPrelude codeArtsTextPhase = iota
	codeArtsTextThinking
	codeArtsTextWrappedAnswer
	codeArtsTextAnswer
)

// Some DeepSeek responses carry prompt delimiters in content. Interpret them
// only at the start of the answer (or at the end of a recognized envelope).
// Buffer only an ambiguous tag suffix or a short prefix, never the response.
type codeArtsInlineReasoning struct {
	phase           codeArtsTextPhase
	pending         string
	thinkingEnd     string
	thinkingHasText bool
	wrapped         bool
	sawMarkup       bool
}

var codeArtsLeadingTags = [...]string{
	assistantOpen, assistantClose, thinkingOpen, thinkingClose, thinkOpen, thinkClose,
}

func (p *codeArtsInlineReasoning) normalize(delta rawObj, finish bool) {
	if p.phase == codeArtsTextAnswer {
		return
	}
	raw, hasContent := delta["content"]
	var original string
	if hasContent && json.Unmarshal(raw, &original) != nil {
		return
	}
	answer, reasoning := p.split(original, finish)
	if answer != original {
		if answer == "" {
			delete(delta, "content")
		} else {
			delta["content"], _ = json.Marshal(answer)
		}
	}
	if reasoning != "" {
		delta["reasoning_content"], _ = json.Marshal(rawString(delta["reasoning_content"]) + reasoning)
	}
}

func (p *codeArtsInlineReasoning) split(text string, finish bool) (string, string) {
	text = p.pending + text
	p.pending = ""
	var answer, reasoning strings.Builder
	for text != "" {
		switch p.phase {
		case codeArtsTextPrelude:
			probe := strings.TrimLeftFunc(text, unicode.IsSpace)
			tag, partial := codeArtsLeadingTag(probe)
			if tag == assistantOpen && !p.wrapped {
				following := strings.TrimLeftFunc(probe[len(tag):], unicode.IsSpace)
				if next, possible := codeArtsLeadingTag(following); next == "" {
					// A bare XML element that contains ordinary text is valid prose.
					if possible && !finish && len(text) <= maxTextPrelude {
						p.pending = strings.Clone(text)
						return answer.String(), reasoning.String()
					}
					if !possible || !finish {
						tag = ""
					}
				}
			}
			if tag != "" {
				p.sawMarkup = true
				switch tag {
				case assistantOpen:
					p.wrapped = true
				case assistantClose:
					p.wrapped = false
				case thinkingOpen, thinkOpen:
					p.thinkingEnd = "</" + tag[1:]
					p.phase = codeArtsTextThinking
					p.thinkingHasText = false
				}
				text = probe[len(tag):]
				continue
			}
			if partial {
				if !finish && len(text) <= maxTextPrelude {
					p.pending = strings.Clone(text)
					return answer.String(), reasoning.String()
				}
				if p.sawMarkup && (finish || probe == "") {
					return answer.String(), reasoning.String()
				}
			}
			p.phase = codeArtsTextAnswer
			if p.wrapped {
				p.phase = codeArtsTextWrappedAnswer
			}
		case codeArtsTextThinking:
			part, rest, closed := cutCodeArtsTextTag(text, p.thinkingEnd)
			if !closed && finish {
				part += rest
			}
			if p.thinkingHasText || strings.TrimSpace(part) != "" {
				p.thinkingHasText = true
				reasoning.WriteString(part)
			}
			if !closed {
				if !finish {
					p.pending = strings.Clone(rest)
				}
				return answer.String(), reasoning.String()
			}
			p.phase, text = codeArtsTextPrelude, rest
		case codeArtsTextWrappedAnswer:
			answer.WriteString(p.wrappedAnswer(text, finish))
			return answer.String(), reasoning.String()
		case codeArtsTextAnswer:
			answer.WriteString(text)
			return answer.String(), reasoning.String()
		}
	}
	return answer.String(), reasoning.String()
}

// An envelope ends only at the end of the reply. An identical tag within the
// answer may be literal XML, so hold a candidate closing suffix until the next
// text or the terminal event disambiguates it.
func (p *codeArtsInlineReasoning) wrappedAnswer(text string, finish bool) string {
	trimmed := strings.TrimRightFunc(text, unicode.IsSpace)
	if part, closed := strings.CutSuffix(trimmed, assistantClose); closed {
		if finish {
			return part
		}
		if tail := text[len(part):]; len(tail) <= maxTextPrelude {
			p.pending = strings.Clone(tail)
			return part
		}
		return text
	}
	part, tail := splitCodeArtsTextTagPrefix(text, assistantClose)
	if finish {
		if strings.HasPrefix(tail, "</assistant") {
			return part
		}
		return text
	}
	p.pending = strings.Clone(tail)
	return part
}

func codeArtsLeadingTag(text string) (tag string, partial bool) {
	for _, tag := range codeArtsLeadingTags {
		if strings.HasPrefix(text, tag) {
			return tag, false
		}
		partial = partial || strings.HasPrefix(tag, text)
	}
	return "", partial
}

// cutCodeArtsTextTag returns a complete delimiter, or keeps only the longest
// possible beginning of that delimiter at the end of the current chunk.
func cutCodeArtsTextTag(text, delimiter string) (head, rest string, found bool) {
	if head, rest, found := strings.Cut(text, delimiter); found {
		return head, rest, true
	}
	head, rest = splitCodeArtsTextTagPrefix(text, delimiter)
	return head, rest, false
}

func splitCodeArtsTextTagPrefix(text, delimiter string) (head, rest string) {
	for n := min(len(text), len(delimiter)-1); n > 0; n-- {
		if tail := text[len(text)-n:]; strings.HasPrefix(delimiter, tail) {
			return text[:len(text)-n], tail
		}
	}
	return text, ""
}
