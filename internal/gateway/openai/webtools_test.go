package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"wb2api/internal/webtools"
)

const webToolCallSSE = `data: {"id":"u","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_w1","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"golang\"}"}}]},"finish_reason":null}]}

data: {"id":"u","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"id":"u","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}

data: [DONE]

`

const webToolAnswerSSE = `data: {"id":"u","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"The answer is 42."},"finish_reason":null}]}

data: {"id":"u","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"u","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":6,"total_tokens":26}}

data: [DONE]

`

type webUpstream struct {
	mu    sync.Mutex
	calls int
	first []string
	last  string
}

func (u *webUpstream) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.calls++
	n := u.calls
	if n == 1 {
		u.first = append(u.first, string(body))
	} else {
		u.last = string(body)
	}
	u.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	if n == 1 {
		io.WriteString(w, webToolCallSSE)
		return
	}
	io.WriteString(w, webToolAnswerSSE)
}

func (u *webUpstream) snapshot() (int, string, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	first := ""
	if len(u.first) > 0 {
		first = u.first[0]
	}
	return u.calls, first, u.last
}

func TestResponsesRunsWebSearchLocally(t *testing.T) {
	up := &webUpstream{}
	e := newEnv(t, http.HandlerFunc(up.handler), 1, `{"local_web_tools":true}`)
	var ran int32
	webtools.ExecuteHook = func(ctx context.Context, name, args string) (string, bool) {
		atomic.AddInt32(&ran, 1)
		if name != "web_search" || !strings.Contains(args, "golang") {
			t.Errorf("hook got %q %q", name, args)
		}
		return "Search results for: golang\n\n1. Go\n   https://go.dev\n\nCite the sources you used at the end of your answer.", true
	}
	defer func() { webtools.ExecuteHook = nil }()

	rs := httptest.NewServer(&Responses{Chat: e.h})
	defer rs.Close()
	body := `{"model":"glm-5.3","input":"what is go?","tools":[{"type":"web_search"}]}`
	resp, err := http.Post(rs.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), "The answer is 42.") {
		t.Fatalf("final answer missing: %s", out)
	}
	calls, first, last := up.snapshot()
	if calls != 2 {
		t.Fatalf("upstream calls %d", calls)
	}
	if !strings.Contains(first, `"web_search"`) || !strings.Contains(first, `"function"`) {
		t.Fatalf("the first request must carry the installed function: %s", first)
	}
	if !strings.Contains(last, `"role":"tool"`) || !strings.Contains(last, "https://go.dev") {
		t.Fatalf("the follow-up must carry the tool result: %s", last)
	}
	if atomic.LoadInt32(&ran) != 1 {
		t.Fatalf("hook ran %d times", ran)
	}
}

func TestResponsesStreamsWebSearchRounds(t *testing.T) {
	up := &webUpstream{}
	e := newEnv(t, http.HandlerFunc(up.handler), 1, `{"local_web_tools":true}`)
	webtools.ExecuteHook = func(ctx context.Context, name, args string) (string, bool) {
		return "Search results for: golang\n\n1. Go\n   https://go.dev\n\nCite the sources you used at the end of your answer.", true
	}
	defer func() { webtools.ExecuteHook = nil }()

	rs := httptest.NewServer(&Responses{Chat: e.h})
	defer rs.Close()
	body := `{"model":"glm-5.3","input":"what is go?","stream":true,"tools":[{"type":"web_search"}]}`
	resp, err := http.Post(rs.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	text := string(out)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, text)
	}
	for _, want := range []string{"response.web_search_call.completed", "The answer is 42.", "response.completed"} {
		if !strings.Contains(text, want) {
			t.Errorf("stream missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, `"type": "function_call"`) {
		t.Errorf("the internal tool call must not be exposed as a function_call:\n%s", text)
	}
}

// contextContext aliases context.Context to keep the hook signature short.
