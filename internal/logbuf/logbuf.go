// Package logbuf keeps the gateway's recent log lines in memory for the panel's
// log page: a bounded ring fed by a slog handler (which still forwards to the
// normal handler), with level / tag / text filtering and "everything after id N"
// tailing.
package logbuf

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Entry is one log line.
type Entry struct {
	ID    int64  `json:"id"`
	TS    string `json:"ts"`
	Time  string `json:"time"`
	Level string `json:"level"`
	Tag   string `json:"tag"`
	Msg   string `json:"msg"`
}

// Ring is a bounded in-memory log.
type Ring struct {
	mu      sync.Mutex
	max     int
	entries []Entry
	next    int64
}

// New returns a ring holding the most recent max entries.
func New(max int) *Ring {
	if max < 10 {
		max = 2000
	}
	return &Ring{max: max}
}

// tag hints, in priority order (a port of the legacy classification); a record
// can also name its tag explicitly with a "tag" attribute.
var tagHints = []struct {
	tag   string
	words []string
}{
	{"chat", []string{"chat:", "chat done", "request error", "/v1/chat", "/chat/completions", "responses"}},
	{"scheduler", []string{"scheduler", "调度器"}},
	{"tasks", []string{"task", "任务", "打卡", "签到", "travel", "growth"}},
	{"accounts", []string{"account", "账号", "pool", "imported"}},
	{"pricing", []string{"pricing", "定价", "price", "价格"}},
	{"egress", []string{"egress", "出口", "proxy slot"}},
	{"upstream", []string{"upstream", "上游"}},
	{"catalog", []string{"model", "catalog", "模型"}},
	{"auth", []string{"auth", "token", "oauth", "login"}},
	{"settings", []string{"settings", "设置"}},
	{"usage", []string{"usage", "用量"}},
}

func inferTag(msg string) string {
	lower := strings.ToLower(msg)
	for _, h := range tagHints {
		for _, w := range h.words {
			if strings.Contains(lower, w) {
				return h.tag
			}
		}
	}
	return "system"
}

// Add appends a line.
func (r *Ring) Add(level, tag, msg string, at time.Time) Entry {
	if tag == "" {
		tag = inferTag(msg)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	e := Entry{ID: r.next, TS: at.Format("2006-01-02 15:04:05"), Time: at.Format("15:04:05"), Level: level, Tag: tag, Msg: strings.TrimRight(msg, "\n ")}
	r.entries = append(r.entries, e)
	if len(r.entries) > r.max {
		r.entries = append([]Entry(nil), r.entries[len(r.entries)-r.max:]...)
	}
	return e
}

// Query selects entries.
type Query struct {
	After int64  // only entries with a larger id
	Level string // minimum level: INFO | WARN | ERROR ("" = all)
	Tag   string
	Text  string // case-insensitive substring
	Limit int    // newest N (default 500)
}

func levelRank(l string) int {
	switch strings.ToUpper(l) {
	case "ERROR":
		return 3
	case "WARN":
		return 2
	}
	return 1
}

// Select returns matching entries oldest-first, plus the highest id seen.
func (r *Ring) Select(q Query) (out []Entry, last int64) {
	if q.Limit <= 0 || q.Limit > 5000 {
		q.Limit = 500
	}
	min := 0
	if q.Level != "" {
		min = levelRank(q.Level)
	}
	text := strings.ToLower(q.Text)
	r.mu.Lock()
	defer r.mu.Unlock()
	last = r.next
	for _, e := range r.entries {
		if e.ID <= q.After || levelRank(e.Level) < min || (q.Tag != "" && e.Tag != q.Tag) {
			continue
		}
		if text != "" && !strings.Contains(strings.ToLower(e.Msg), text) {
			continue
		}
		out = append(out, e)
	}
	if len(out) > q.Limit {
		out = out[len(out)-q.Limit:]
	}
	return out, last
}

// Clear drops everything (ids keep counting up).
func (r *Ring) Clear() {
	r.mu.Lock()
	r.entries = nil
	r.mu.Unlock()
}

// Tags lists the tags present.
func (r *Ring) Tags() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, e := range r.entries {
		if !seen[e.Tag] {
			seen[e.Tag] = true
			out = append(out, e.Tag)
		}
	}
	return out
}

// ------------------------------------------------------------ slog bridge

type handler struct {
	ring  *Ring
	next  slog.Handler
	attrs []slog.Attr
	group string
}

// NewHandler returns a handler that records into ring and forwards to next
// (which may be nil).
func NewHandler(ring *Ring, next slog.Handler) slog.Handler {
	return &handler{ring: ring, next: next}
}

func (h *handler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next == nil || h.next.Enabled(ctx, l) || l >= slog.LevelInfo
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	tag := ""
	add := func(a slog.Attr) {
		if a.Key == "tag" {
			tag = a.Value.String()
			return
		}
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value.Resolve().Any())
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(func(a slog.Attr) bool { add(a); return true })
	level := "INFO"
	switch {
	case r.Level >= slog.LevelError:
		level = "ERROR"
	case r.Level >= slog.LevelWarn:
		level = "WARN"
	}
	if r.Level >= slog.LevelInfo {
		h.ring.Add(level, tag, b.String(), r.Time)
	}
	if h.next != nil && h.next.Enabled(ctx, r.Level) {
		return h.next.Handle(ctx, r)
	}
	return nil
}

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	nh := *h
	nh.attrs = append(append([]slog.Attr(nil), h.attrs...), as...)
	if h.next != nil {
		nh.next = h.next.WithAttrs(as)
	}
	return &nh
}

func (h *handler) WithGroup(name string) slog.Handler {
	nh := *h
	if h.next != nil {
		nh.next = h.next.WithGroup(name)
	}
	return &nh
}

var timeNow = time.Now
