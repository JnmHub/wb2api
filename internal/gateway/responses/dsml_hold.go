package responses

import (
	"strings"
	"time"
	"unicode/utf8"

	"wb2api/internal/gateway/leak"
)

// Match the chat filter's hold budget. This bounds speculative withholding,
// not the completed response, which the Responses protocol must retain.
const dsmlMaxHold = leak.DefaultMaxHold
const dsmlMaxHoldTime = 20 * time.Second

// dsmlHold scans each byte/tag once. The builder grows amortized-linearly;
// neither appends nor incomplete-block probes rescan/copy the accumulated body.
// A candidate exceeding a budget is emitted literally, never truncated or
// reparsed at Finish (that would contradict text deltas already sent).
type dsmlHold struct {
	buf     strings.Builder
	start   int // consumed prefix, compacted only before appending another chunk
	since   time.Time
	scanned int
	tag     int
	opened  bool
	blocked bool // first complete block starts behind an earlier held candidate
}

func (s *Stream) releaseDSML(out *[]Frame) {
	if s.textHold.buf.Len() > 0 {
		s.textDelta(out, s.textHold.buf.String()[s.textHold.start:])
	}
	s.textHold = dsmlHold{}
}

// expireDSML is checked on every incoming Chunk, including usage-only chunks.
// An independently firing wall-clock release requires caller I/O/timer
// coordination: Stream itself is synchronous and must not emit concurrently.
func (s *Stream) expireDSML(out *[]Frame) {
	if !s.textHold.since.IsZero() && time.Since(s.textHold.since) >= dsmlMaxHoldTime {
		s.releaseDSML(out)
	}
}

// completeEnd incrementally prefilters leak.FindDSMLBlock. Completed tags and
// non-tag body text are never searched again on later chunks.
func (h *dsmlHold) completeEnd() int {
	if h.blocked {
		return 0
	}
	text := h.buf.String()
	for h.scanned < len(text) {
		i := strings.IndexByte(text[h.scanned:], '>')
		if i < 0 {
			h.scanned = len(text)
			break
		}
		end := h.scanned + i + 1
		tag := text[h.tag:end]
		open := strings.IndexByte(tag, '<')
		callsTag := open >= 0 && strings.Contains(tag[open:], "DSML") && strings.HasSuffix(tag, "calls>")
		h.scanned, h.tag = end, end
		close := strings.Index(tag, "</")
		if h.opened && callsTag && close >= 0 && strings.Contains(tag[close+2:], "DSML") {
			return end
		}
		if callsTag {
			h.opened = true
		}
	}
	return 0
}

func (s *Stream) feedText(out *[]Frame, piece string) {
	for piece != "" {
		h := &s.textHold
		if h.start > 0 {
			// Compact once between chunks, never once per tag/text prefix.
			text := h.buf.String()[h.start:]
			since, scanned, tag, opened, blocked := h.since, h.scanned-h.start, max(0, h.tag-h.start), h.opened, h.blocked
			s.textHold = dsmlHold{since: since, scanned: scanned, tag: tag, opened: opened, blocked: blocked}
			h = &s.textHold
			h.buf.WriteString(text)
		}
		if h.buf.Len() == 0 && !strings.Contains(piece, "<") {
			s.textDelta(out, piece)
			return
		}
		if h.since.IsZero() {
			h.since = time.Now()
		}
		n := min(len(piece), dsmlMaxHold-h.buf.Len())
		for n > 0 && n < len(piece) && !utf8.RuneStart(piece[n]) {
			n--
		}
		if n == 0 {
			s.releaseDSML(out)
			continue
		}
		h.buf.WriteString(piece[:n])
		piece = piece[n:]
		s.drainText(out)
		if s.textHold.buf.Len()-s.textHold.start == dsmlMaxHold {
			s.releaseDSML(out)
		}
	}
}

func (s *Stream) drainText(out *[]Frame) {
	for s.textHold.buf.Len() > 0 {
		h := &s.textHold
		text := h.buf.String()
		idx := strings.IndexByte(text[h.start:], '<')
		if idx >= 0 {
			idx += h.start
		}
		if idx < 0 {
			s.releaseDSML(out)
			return
		}
		if end := h.completeEnd(); end > 0 {
			bs, be, ok := leak.FindDSMLBlock(text[h.start:end])
			bs, be = bs+h.start, be+h.start
			if ok && bs == idx {
				if idx > h.start {
					s.textDelta(out, text[h.start:idx])
				}
				calls, _ := s.env.parseDSML(text[bs:be])
				s.dsmlCalls = append(s.dsmlCalls, calls...)
				h.start, h.scanned, h.tag, h.opened = be, be, be, false
				if be == len(text) {
					s.textHold = dsmlHold{}
					return
				}
				continue
			}
			// The legacy policy keeps the earlier candidate until EOF.
			// Don't repeatedly regex-search this same complete block.
			h.blocked = ok
		}
		cand := runeSlice(text, idx, 30)
		isCand := strings.Contains(cand, "DSML") ||
			(utf8.RuneCountInString(cand) < 10 && !strings.ContainsAny(cand, " \t\n>"))
		if isCand {
			s.textDelta(out, text[h.start:idx])
			h.start = idx
			return
		}
		if next := strings.IndexByte(text[idx+1:], '<'); next >= 0 {
			flush := idx + 1 + next
			s.textDelta(out, text[h.start:flush])
			h.start, h.scanned, h.tag, h.opened, h.blocked = flush, flush, flush, false, false
			h.since = time.Now()
		} else {
			s.releaseDSML(out)
			return
		}
	}
}
