// Package activity keeps the structured history of the automated daily
// activities (CN check-in, CN activity report, INTL daily chat): one JSON line
// per real attempt, so "did this account get its check-in yesterday?" has an
// answer after a restart. Only operational metadata is stored — never tokens,
// cookies or request bodies, and free text is truncated and redacted.
package activity

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Tasks and triggers.
const (
	TaskCheckin   = "checkin"
	TaskDailyChat = "daily_chat"
	TaskReport    = "activity_report"
)

const (
	historyName   = "activity_history.jsonl"
	maxRecords    = 5000
	maxAgeDays    = 90
	maxMessage    = 200
	compactBytes  = 2_000_000
	compactEvery  = 6 * time.Hour
	defaultLimit  = 100
	maxLimit      = 1000
	defaultWindow = 7
)

// Row is one attempt.
type Row struct {
	TS       string `json:"ts"`
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Realm    string `json:"realm"`
	Task     string `json:"task"`
	Trigger  string `json:"trigger"`
	OK       bool   `json:"ok"`
	Message  string `json:"message"`
}

// Log is the history file.
type Log struct {
	Dir string
	Now func() time.Time

	mu          sync.Mutex
	lastCompact time.Time
}

func (l *Log) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Log) path() string { return filepath.Join(l.Dir, historyName) }

var secretRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`(?i)\b(?:access[_-]?token|refresh[_-]?token|api[_-]?key|apikey|authorization|cookie|set-cookie|password|secret|token)\b\s*[:=]\s*\S+`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}\b`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{12,}\b`),
	regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`),
	regexp.MustCompile(`\b[A-Za-z0-9+/]{40,}={0,2}\b`),
}

// SafeMessage squeezes upstream free text into one short, redacted line.
func SafeMessage(text string) string {
	line := strings.Join(strings.Fields(text), " ")
	for _, re := range secretRes {
		line = re.ReplaceAllString(line, "[redacted]")
	}
	r := []rune(line)
	if len(r) > maxMessage {
		r = r[:maxMessage]
	}
	return string(r)
}

func normTrigger(t string) string {
	switch t {
	case "scheduler", "manual", "account_add", "account_import":
		return t
	}
	return "unknown"
}

// Record appends one attempt. A write failure never changes the outcome of the
// action that was recorded.
func (l *Log) Record(r Row) {
	if l == nil || l.Dir == "" {
		return
	}
	r.TS = l.now().Format("2006-01-02 15:04:05")
	r.Trigger = normTrigger(r.Trigger)
	r.Message = SafeMessage(r.Message)
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = os.MkdirAll(l.Dir, 0o755)
	f, err := os.OpenFile(l.path(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	st, _ := f.Stat()
	_ = f.Close()
	if (st != nil && st.Size() > compactBytes) || l.now().Sub(l.lastCompact) > compactEvery {
		l.lastCompact = l.now()
		l.compactLocked()
	}
}

func (l *Log) readLocked() []Row {
	f, err := os.Open(l.path())
	if err != nil {
		return nil
	}
	defer f.Close()
	var rows []Row
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var r Row
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.TS != "" {
			r.Message = SafeMessage(r.Message)
			rows = append(rows, r)
		}
	}
	return rows
}

func (l *Log) compactLocked() {
	rows := l.readLocked()
	cutoff := l.now().AddDate(0, 0, -maxAgeDays).Format("2006-01-02 15:04:05")
	var keep []Row
	for _, r := range rows {
		if r.TS >= cutoff {
			keep = append(keep, r)
		}
	}
	if len(keep) > maxRecords {
		keep = keep[len(keep)-maxRecords:]
	}
	var sb strings.Builder
	for _, r := range keep {
		b, _ := json.Marshal(r)
		sb.Write(b)
		sb.WriteByte('\n')
	}
	tmp := l.path() + ".tmp"
	if os.WriteFile(tmp, []byte(sb.String()), 0o644) == nil {
		_ = os.Rename(tmp, l.path())
	}
}

// Filter selects rows.
type Filter struct {
	Range  string // today | 1d | 7d | 30d | 90d | all (default 7d)
	UID    string
	Task   string
	Result string // ok | failed
	Limit  int
}

// Result of Query.
type Result struct {
	Rows      []Row `json:"rows"`
	Total     int   `json:"total"`
	Limit     int   `json:"limit"`
	Truncated bool  `json:"truncated"`
}

// Query returns matching rows, newest first.
func (l *Log) Query(f Filter) Result {
	if f.Limit <= 0 {
		f.Limit = defaultLimit
	}
	if f.Limit > maxLimit {
		f.Limit = maxLimit
	}
	now := l.now()
	cutoff := ""
	switch f.Range {
	case "today":
		cutoff = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Format("2006-01-02 15:04:05")
	case "all":
	case "1d":
		cutoff = now.AddDate(0, 0, -1).Format("2006-01-02 15:04:05")
	case "30d":
		cutoff = now.AddDate(0, 0, -30).Format("2006-01-02 15:04:05")
	case "90d":
		cutoff = now.AddDate(0, 0, -90).Format("2006-01-02 15:04:05")
	default:
		cutoff = now.AddDate(0, 0, -defaultWindow).Format("2006-01-02 15:04:05")
	}
	l.mu.Lock()
	rows := l.readLocked()
	l.mu.Unlock()
	var matched []Row
	for _, r := range rows {
		if r.TS < cutoff || (f.UID != "" && r.UID != f.UID) || (f.Task != "" && r.Task != f.Task) {
			continue
		}
		if f.Result == "ok" && !r.OK || f.Result == "failed" && r.OK {
			continue
		}
		matched = append(matched, r)
	}
	sort.SliceStable(matched, func(i, j int) bool { return matched[i].TS > matched[j].TS })
	res := Result{Total: len(matched), Limit: f.Limit, Rows: matched}
	if len(matched) > f.Limit {
		res.Rows, res.Truncated = matched[:f.Limit], true
	}
	if res.Rows == nil {
		res.Rows = []Row{}
	}
	return res
}
