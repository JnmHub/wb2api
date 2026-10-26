package ops

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCheckinOutcomeClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		code      string
		msg       string
		ok        bool
		retryable bool
	}{
		{"plain success", 200, "0", "ok", true, false},
		{"already checked in", 200, "10001", "今日已签到", true, false},
		{"already checked in english", 200, "10001", "already checked in", true, false},
		{"duplicate in flight", 200, "10001", "请求处理中，请勿重复操作", false, true},
		{"busy", 200, "10001", "操作频繁，请稍后再试", false, true},
		{"unknown business failure", 200, "40004", "参数错误", false, false},
		{"http 429", 429, "", "too many requests", false, true},
		{"http 500", 500, "", "", false, true},
		{"http 403", 403, "", "forbidden", false, false},
		{"http 200 without code", 200, "", "ok", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, retryable := checkinOutcome(tc.status, tc.code, tc.msg)
			if ok != tc.ok || retryable != tc.retryable {
				t.Fatalf("checkinOutcome(%d,%q,%q) = (%v,%v), want (%v,%v)",
					tc.status, tc.code, tc.msg, ok, retryable, tc.ok, tc.retryable)
			}
		})
	}
}

func TestCheckinDuplicateInFlightDoesNotMarkDone(t *testing.T) {
	e := newEnv(t, "cn", nil)
	e.vendor.json("/v2/billing/meter/daily-checkin", `{"code":10001,"msg":"请求处理中，请勿重复操作"}`)
	res := e.c.Checkin(context.Background(), e.acc(), "test")
	if res.OK {
		t.Fatalf("duplicate in-flight checkin reported success: %+v", res)
	}
	if !strings.Contains(res.Error, "稍后重试") {
		t.Fatalf("want retryable hint, got %+v", res)
	}
	if !e.c.CanCheckin(e.acc()) {
		t.Fatal("account was marked as checked in")
	}
}

func TestCheckinAlreadyDoneIsIdempotentSuccess(t *testing.T) {
	e := newEnv(t, "cn", nil)
	e.vendor.json("/v2/billing/meter/daily-checkin", `{"code":10001,"msg":"今日已签到"}`)
	res := e.c.Checkin(context.Background(), e.acc(), "test")
	if !res.OK {
		t.Fatalf("already-checked answer should be success: %+v", res)
	}
	if e.c.CanCheckin(e.acc()) {
		t.Fatal("idempotent success did not mark the day")
	}
}

// webACP starts the sandbox ACP endpoint the web turn attaches to.
func webACP(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Acp-Connection-Id", "conn")
			io.WriteString(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hi"}}}}`+"\n\n")
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// wireConversation registers session/status/stop/delete handlers for one id.
func wireConversation(e *env, convID, acpURL, status string) {
	e.vendor.h["/console/as/conversations/"+convID+"/session"] = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":0,"data":{"link":"`+acpURL+`/acp","token":"tok","sessionId":"s-1","cwd":"/workspace"}}`)
	}
	e.vendor.h["/console/as/conversations/"+convID] = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			io.WriteString(w, `{"code":0}`)
			return
		}
		io.WriteString(w, `{"code":0,"data":{"status":"`+status+`"}}`)
	}
	e.vendor.h["/console/as/conversations/"+convID+"/stop"] = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":0}`)
	}
}

func callsMatching(e *env, want string) int {
	e.vendor.mu.Lock()
	defer e.vendor.mu.Unlock()
	n := 0
	for _, call := range e.vendor.calls {
		if call == want {
			n++
		}
	}
	return n
}

func TestWebDailyChatReleasesConversationOnSuccess(t *testing.T) {
	e := newEnv(t, "intl", nil)
	_ = e.c.Settings.SetDailyChatWebAttempts(1)
	acp := webACP(t)
	e.vendor.json("/console/as/conversations/", `{"code":0,"data":{"id":"conv-ok"}}`)
	wireConversation(e, "conv-ok", acp.URL, "completed")

	res := e.c.DailyChatWeb(context.Background(), e.acc())
	if !res.OK {
		t.Fatalf("turn failed: %+v", res)
	}
	if n := callsMatching(e, "POST /console/as/conversations/conv-ok/stop"); n != 1 {
		t.Fatalf("stop called %d times", n)
	}
	if n := callsMatching(e, "DELETE /console/as/conversations/conv-ok"); n != 0 {
		t.Fatalf("successful conversation deleted %d times", n)
	}
}

func TestWebDailyChatDeletesFailedConversation(t *testing.T) {
	e := newEnv(t, "intl", nil)
	_ = e.c.Settings.SetDailyChatWebAttempts(1)
	acp := webACP(t)
	e.vendor.json("/console/as/conversations/", `{"code":0,"data":{"id":"conv-bad"}}`)
	wireConversation(e, "conv-bad", acp.URL, "failed")

	res := e.c.DailyChatWeb(context.Background(), e.acc())
	if res.OK {
		t.Fatalf("failed conversation reported success: %+v", res)
	}
	if n := callsMatching(e, "POST /console/as/conversations/conv-bad/stop"); n != 1 {
		t.Fatalf("stop called %d times", n)
	}
	if n := callsMatching(e, "DELETE /console/as/conversations/conv-bad"); n != 1 {
		t.Fatalf("delete called %d times", n)
	}
}

func TestWebDailyChatClearsOldestConversationOnCap(t *testing.T) {
	e := newEnv(t, "intl", nil)
	_ = e.c.Settings.SetDailyChatWebAttempts(1)
	acp := webACP(t)
	var mu sync.Mutex
	creates := 0
	e.vendor.h["/console/as/conversations/"] = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"code":0,"data":{"conversations":[{"id":"old-2","createdAt":200},{"id":"old-1","createdAt":100}]}}`)
			return
		}
		mu.Lock()
		creates++
		n := creates
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"code":10105,"msg":"会话数量已达上限"}`)
			return
		}
		io.WriteString(w, `{"code":0,"data":{"id":"conv-new"}}`)
	}
	e.vendor.h["/console/as/conversations/old-1"] = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":0}`)
	}
	wireConversation(e, "conv-new", acp.URL, "completed")

	res := e.c.DailyChatWeb(context.Background(), e.acc())
	if !res.OK {
		t.Fatalf("turn failed after cleanup: %+v", res)
	}
	if n := callsMatching(e, "DELETE /console/as/conversations/old-1"); n != 1 {
		t.Fatalf("oldest conversation deleted %d times", n)
	}
	if n := callsMatching(e, "DELETE /console/as/conversations/old-2"); n != 0 {
		t.Fatalf("newer conversation deleted %d times", n)
	}
	mu.Lock()
	got := creates
	mu.Unlock()
	if got != 2 {
		t.Fatalf("create attempts = %d", got)
	}
}

func TestListWebConversationsIsOldestFirst(t *testing.T) {
	e := newEnv(t, "intl", nil)
	e.vendor.h["/console/as/conversations/"] = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":0,"data":{"conversations":[{"id":"b","createdAt":20},{"id":"a","createdAt":10}]}}`)
	}
	ids := e.c.listWebConversations(context.Background(), e.acc())
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("ids = %v", ids)
	}
}

func TestDailyChatBackoffIsLinearAndBounded(t *testing.T) {
	want := []time.Duration{2 * time.Second, 4 * time.Second, 6 * time.Second, 8 * time.Second}
	for i, w := range want {
		if got := dailyChatBackoff(i + 1); got != w {
			t.Fatalf("backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
	if got := dailyChatBackoff(20); got != 30*time.Second {
		t.Fatalf("capped backoff(20) = %v", got)
	}
	if got := dailyChatBackoff(0); got != 0 {
		t.Fatalf("backoff(0) = %v", got)
	}
}

func TestDailyChatBudgetOverride(t *testing.T) {
	t.Setenv("WB_DAILY_CHAT_BUDGET_MS", "90000")
	if got := dailyChatBudget(); got != 90*time.Second {
		t.Fatalf("budget = %v", got)
	}
	if ms, ok := envMillis("WB_DAILY_CHAT_BUDGET_MS"); !ok || ms != 90000 {
		t.Fatalf("envMillis = %d,%v", ms, ok)
	}
	t.Setenv("WB_DAILY_CHAT_BUDGET_MS", "nonsense")
	if got := dailyChatBudget(); got != time.Duration(8)*time.Minute {
		t.Fatalf("invalid override budget = %v", got)
	}
}

var _ = json.Marshal
