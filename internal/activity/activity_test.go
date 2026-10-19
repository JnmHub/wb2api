package activity

import (
	"strings"
	"testing"
	"time"
)

func TestRecordQueryAndRedaction(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local)
	l := &Log{Dir: t.TempDir(), Now: func() time.Time { return now }}
	l.Record(Row{UID: "u1", Realm: "cn", Task: TaskCheckin, Trigger: "scheduler", OK: true, Message: "ok"})
	l.Record(Row{UID: "u2", Realm: "intl", Task: TaskDailyChat, Trigger: "weird", OK: false,
		Message: "HTTP 401: Authorization: Bearer abcdefghijklmnop12345 token=eyJabcdefghij.klmnopqrstuv.wxyz1234 " + strings.Repeat("x", 300)})
	l.Now = func() time.Time { return now.AddDate(0, 0, -10) }
	l.Record(Row{UID: "u1", Task: TaskCheckin, OK: true, Message: "old"})
	l.Now = func() time.Time { return now }

	all := l.Query(Filter{})
	if all.Total != 2 {
		t.Fatalf("the default window is 7 days: %+v", all)
	}
	bad := l.Query(Filter{Result: "failed"})
	if bad.Total != 1 || bad.Rows[0].Trigger != "unknown" {
		t.Fatalf("%+v", bad)
	}
	msg := bad.Rows[0].Message
	if strings.Contains(msg, "abcdefghijklmnop12345") || strings.Contains(msg, "eyJ") || len([]rune(msg)) > 200 {
		t.Fatalf("secrets and long text must not reach the history: %q", msg)
	}
	if got := l.Query(Filter{Range: "all", UID: "u1"}); got.Total != 2 {
		t.Fatalf("range all sees the old row: %+v", got)
	}
	if got := l.Query(Filter{Range: "all", Limit: 1}); !got.Truncated || len(got.Rows) != 1 || got.Total != 3 {
		t.Fatalf("limit: %+v", got)
	}
	if got := l.Query(Filter{Task: TaskCheckin, Result: "ok"}); got.Total != 1 {
		t.Fatalf("%+v", got)
	}
}
