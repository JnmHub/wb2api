package openai

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/gateway/leak"
	"wb2api/internal/pool"
	"wb2api/internal/store/accounts"
	"wb2api/internal/store/settings"
	"wb2api/internal/upstream"
)

type env struct {
	h      *Handler
	srv    *httptest.Server // the gateway
	events chan UsageEvent
}

func newEnv(t *testing.T, up http.Handler, nAccounts int, settingsJSON string) *env {
	t.Helper()
	upSrv := httptest.NewServer(up)
	t.Cleanup(upSrv.Close)
	dir := t.TempDir()
	exp := time.Now().Add(24 * time.Hour).Unix()
	var recs []*accounts.Record
	for i := 0; i < nAccounts; i++ {
		r := accounts.FromMap(map[string]any{
			"uid": fmt.Sprintf("acct-%d", i), "accessToken": fmt.Sprintf("token-%d", i),
			"realm": "intl", "expiresAt": exp,
		}, "", time.Now())
		recs = append(recs, r)
	}
	if settingsJSON != "" {
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settingsJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e := &env{events: make(chan UsageEvent, 64)}
	e.h = &Handler{
		Pool: pool.New(recs), Up: upstream.NewManager(upstream.DefaultTimeouts()),
		Settings: settings.Open(dir), UpstreamOverride: upSrv.URL, MaxAttempts: 5,
		OnUsage: func(ev UsageEvent) { e.events <- ev },
	}
	e.srv = httptest.NewServer(e.h)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) post(t *testing.T, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (e *env) event(t *testing.T) UsageEvent {
	t.Helper()
	select {
	case ev := <-e.events:
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("no usage event")
		return UsageEvent{}
	}
}

// sseUpstream streams the given content pieces, with the upstream's habit of
// padding deltas with empty fields, then a finish chunk and a usage chunk.
func sseUpstream(pieces ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, p := range pieces {
			b, _ := json.Marshal(p)
			fmt.Fprintf(w, `data: {"id":"up-1","object":"chat.completion.chunk","created":1790000000,"model":"m","choices":[{"index":0,"delta":{"content":%s,"reasoning_content":"","tool_calls":[]},"finish_reason":""}]}`+"\n\n", b)
			fl.Flush()
		}
		fmt.Fprint(w, `data: {"id":"up-1","object":"chat.completion.chunk","created":1790000000,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, `data: {"id":"up-1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}
}

type parsed struct {
	content   string
	toolCalls []map[string]any
	finish    string
	done      bool
	raw       []string
}

func readSSE(t *testing.T, resp *http.Response) parsed {
	t.Helper()
	defer resp.Body.Close()
	var p parsed
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		p.raw = append(p.raw, data)
		if data == "[DONE]" {
			p.done = true
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					Content   string           `json:"content"`
					ToolCalls []map[string]any `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatalf("client received invalid JSON %q: %v", data, err)
		}
		for _, ch := range c.Choices {
			p.content += ch.Delta.Content
			p.toolCalls = append(p.toolCalls, ch.Delta.ToolCalls...)
			if ch.FinishReason != nil {
				p.finish = *ch.FinishReason
			}
		}
	}
	return p
}

const chatBody = `{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"hi"}]}`

func TestStreamPassThroughStripsNoise(t *testing.T) {
	e := newEnv(t, sseUpstream("Hello ", "world"), 1, "")
	resp := e.post(t, chatBody, nil)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type %q", ct)
	}
	p := readSSE(t, resp)
	if p.content != "Hello world" || p.finish != "stop" || !p.done {
		t.Fatalf("got %+v", p)
	}
	for _, r := range p.raw {
		if strings.Contains(r, `"reasoning_content"`) || strings.Contains(r, `"tool_calls":[]`) {
			t.Errorf("upstream padding leaked to client: %s", r)
		}
	}
	ev := e.event(t)
	if ev.Outcome != "completed" || ev.Account != "acct-0" || !ev.Stream || ev.Estimated {
		t.Fatalf("bad usage event %+v", ev)
	}
	if !strings.Contains(string(ev.Usage), `"total_tokens":15`) {
		t.Errorf("usage not captured: %s", ev.Usage)
	}
}

func TestThinkingTagDoesNotStall(t *testing.T) {
	// The upstream sends <thinking> and then keeps the connection open before
	// the final chunk. The client must see the text after the tag while the
	// stream is still running (legacy withheld it until the end).
	release := make(chan struct{})
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		for _, p := range []string{"intro ", "<thinking>", "reasoning text "} {
			b, _ := json.Marshal(p)
			fmt.Fprintf(w, `data: {"id":"u","choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`+"\n\n", b)
			fl.Flush()
		}
		<-release
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	e := newEnv(t, up, 1, "")
	resp := e.post(t, chatBody, nil)
	defer resp.Body.Close()
	defer close(release)
	got := make(chan string, 1)
	go func() {
		var sb strings.Builder
		br := bufio.NewReader(resp.Body)
		for !strings.Contains(sb.String(), "reasoning text") {
			line, err := br.ReadString('\n')
			if err != nil {
				break
			}
			if strings.HasPrefix(line, "data: {") {
				var c struct {
					Choices []struct {
						Delta struct {
							Content string `json:"content"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &c) == nil && len(c.Choices) > 0 {
					sb.WriteString(c.Choices[0].Delta.Content)
				}
			}
		}
		got <- sb.String()
	}()
	select {
	case s := <-got:
		if !strings.Contains(s, "<thinking>") || !strings.Contains(s, "reasoning text") {
			t.Fatalf("stream did not deliver the text: %q", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("text after <thinking> was withheld while the stream was open (the legacy stall)")
	}
}

func TestLegacyModeStillStalls(t *testing.T) {
	// Guards the knob: Legacy must reproduce the old hold-until-end behaviour.
	release := make(chan struct{})
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		for _, p := range []string{"<thinking>", "later text"} {
			b, _ := json.Marshal(p)
			fmt.Fprintf(w, `data: {"id":"u","choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`+"\n\n", b)
			fl.Flush()
		}
		<-release
	})
	e := newEnv(t, up, 1, "")
	e.h.FilterMode = leak.Legacy
	resp := e.post(t, chatBody, nil)
	defer resp.Body.Close()
	defer close(release)
	seen := make(chan struct{}, 1)
	go func() {
		br := bufio.NewReader(resp.Body)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if strings.Contains(line, "later text") {
				seen <- struct{}{}
				return
			}
		}
	}()
	select {
	case <-seen:
		t.Fatal("legacy mode should have withheld the text")
	case <-time.After(500 * time.Millisecond):
	}
}

func TestLeakedToolCallIsRecoveredInStream(t *testing.T) {
	e := newEnv(t, sseUpstream("Running it.\n", "<run_code>", `<parameter name="code">print(1)</parameter>`, "</run_code>"), 1, "")
	p := readSSE(t, e.post(t, chatBody, nil))
	if len(p.toolCalls) != 1 {
		t.Fatalf("want 1 recovered tool call, got %+v", p)
	}
	fn := p.toolCalls[0]["function"].(map[string]any)
	if fn["name"] != "run_code" || !strings.Contains(fn["arguments"].(string), "print(1)") {
		t.Fatalf("wrong call %v", fn)
	}
	if p.finish != "tool_calls" || !p.done {
		t.Fatalf("finish=%q done=%v", p.finish, p.done)
	}
	if strings.Contains(p.content, "<run_code") || strings.Contains(p.content, "parameter") {
		t.Errorf("markup left in prose: %q", p.content)
	}
	if !strings.Contains(p.content, "Running it.") {
		t.Errorf("prose lost: %q", p.content)
	}
}

func TestFailoverOn429CoolsOnlyThatModel(t *testing.T) {
	var calls atomic.Int32
	var mu sync.Mutex
	seenTokens := []string{}
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenTokens = append(seenTokens, r.Header.Get("Authorization"))
		mu.Unlock()
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"code":6004,"msg":"usage exceeds frequency limit"}`)
			return
		}
		sseUpstream("ok")(w, r)
	})
	e := newEnv(t, up, 2, "")
	p := readSSE(t, e.post(t, chatBody, nil))
	if p.content != "ok" || !p.done {
		t.Fatalf("failover failed: %+v", p)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seenTokens) != 2 || seenTokens[0] == seenTokens[1] {
		t.Fatalf("expected two different accounts to be tried, got %v", seenTokens)
	}
	// the throttled account still serves another model
	var throttled *pool.Account
	for _, a := range e.h.Pool.All() {
		if "Bearer "+a.Record().AccessToken == seenTokens[0] {
			throttled = a
		}
	}
	if throttled.Ready("glm-5.3", time.Now()) {
		t.Error("throttled account should be cooling for glm-5.3")
	}
	if !throttled.Ready("other-model", time.Now()) {
		t.Error("a model-scoped 429 must not park the whole account")
	}
}

func TestForbiddenIsPassedThroughWithoutRotation(t *testing.T) {
	var calls atomic.Int32
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"code":11140,"msg":"account restricted"}`)
	})
	e := newEnv(t, up, 3, "")
	resp := e.post(t, chatBody, nil)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 403 || !strings.Contains(string(b), "11140") {
		t.Fatalf("got %d %s", resp.StatusCode, b)
	}
	if calls.Load() != 1 {
		t.Fatalf("a 403 must not be replayed on other accounts, upstream saw %d calls", calls.Load())
	}
}

func TestAllAccountsRateLimited(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	e := newEnv(t, up, 2, "")
	resp := e.post(t, chatBody, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestNoAccountsIs503(t *testing.T) {
	e := newEnv(t, sseUpstream("x"), 0, "")
	if resp := e.post(t, chatBody, nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestAuthAndKeyModelRestriction(t *testing.T) {
	cfg := `{"api_keys":[
	  {"id":"a","name":"all","key":"sk-all"},
	  {"id":"b","name":"ds","key":"sk-ds","models":"deepseek*"}]}`
	e := newEnv(t, sseUpstream("ok"), 1, cfg)
	if r := e.post(t, chatBody, nil); r.StatusCode != 401 {
		t.Fatalf("missing key: %d", r.StatusCode)
	}
	if r := e.post(t, chatBody, map[string]string{"Authorization": "Bearer wrong"}); r.StatusCode != 401 {
		t.Fatalf("wrong key: %d", r.StatusCode)
	}
	if r := e.post(t, chatBody, map[string]string{"Authorization": "Bearer sk-ds"}); r.StatusCode != 400 {
		t.Fatalf("restricted model must be refused locally: %d", r.StatusCode)
	}
	if p := readSSE(t, e.post(t, chatBody, map[string]string{"Authorization": "Bearer sk-all"})); p.content != "ok" {
		t.Fatalf("valid key rejected: %+v", p)
	}
	ev := e.event(t)
	if ev.KeyID != "a" {
		t.Errorf("usage should be attributed to key a, got %q", ev.KeyID)
	}
}

func TestBodyTooLarge(t *testing.T) {
	e := newEnv(t, sseUpstream("x"), 1, "")
	e.h.MaxBody = 1024
	big := `{"model":"m","stream":true,"messages":[{"role":"user","content":"` + strings.Repeat("a", 4096) + `"}]}`
	if r := e.post(t, big, nil); r.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d", r.StatusCode)
	}
}

func TestBadJSON(t *testing.T) {
	e := newEnv(t, sseUpstream("x"), 1, "")
	if r := e.post(t, `not json`, nil); r.StatusCode != 400 {
		t.Fatalf("status %d", r.StatusCode)
	}
}

func TestNonStreamAggregatesAndRepairsLeakedCall(t *testing.T) {
	e := newEnv(t, sseUpstream("Hi ", "there"), 1, "")
	resp := e.post(t, `{"model":"glm-5.3","stream":false,"messages":[]}`, nil)
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "Hi there" || out.Choices[0].FinishReason != "stop" || out.Usage["total_tokens"] == nil {
		t.Fatalf("got %+v", out)
	}

	e2 := newEnv(t, sseUpstream("<run_code>", `<parameter name="code">x</parameter>`, "</run_code>"), 1, "")
	resp2 := e2.post(t, `{"model":"glm-5.3","stream":false}`, nil)
	var out2 struct {
		Choices []struct {
			Message struct {
				ToolCalls []map[string]any `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&out2)
	if len(out2.Choices) != 1 || len(out2.Choices[0].Message.ToolCalls) != 1 || out2.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("leaked call not repaired: %+v", out2)
	}
}

func TestMissingUsageIsEstimatedAndFlagged(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `data: {"id":"u","choices":[{"index":0,"delta":{"content":"你好世界 hello"},"finish_reason":null}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	e := newEnv(t, up, 1, "")
	readSSE(t, e.post(t, chatBody, nil))
	ev := e.event(t)
	if !ev.Estimated || !strings.Contains(string(ev.Usage), `"prompt_tokens":0`) {
		t.Fatalf("estimated usage must be flagged and must not invent a prompt count: %+v %s", ev, ev.Usage)
	}
}

func TestClientDisconnectCancelsUpstreamAndReleasesAccount(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"u","choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}]}`+"\n\n")
		fl.Flush()
		<-r.Context().Done()
		close(upstreamCanceled)
	})
	e := newEnv(t, up, 1, "")
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL, strings.NewReader(chatBody))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(resp.Body)
	_, _ = br.ReadString('\n') // first data line
	resp.Body.Close()          // client goes away mid-stream
	select {
	case <-upstreamCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request was not cancelled after the client disconnected")
	}
	ev := e.event(t)
	if ev.Outcome != "client_aborted" {
		t.Errorf("outcome %q", ev.Outcome)
	}
	deadline := time.Now().Add(2 * time.Second)
	for e.h.Pool.All()[0].Inflight() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("account in-flight count leaked after disconnect")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEachAccountUsesItsOwnConnectionPool(t *testing.T) {
	// Two accounts must never share a TCP connection to the upstream.
	var mu sync.Mutex
	remoteByToken := map[string]map[string]bool{}
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tok := r.Header.Get("Authorization")
		if remoteByToken[tok] == nil {
			remoteByToken[tok] = map[string]bool{}
		}
		remoteByToken[tok][r.RemoteAddr] = true
		mu.Unlock()
		sseUpstream("x")(w, r)
	})
	e := newEnv(t, up, 3, "")
	for i := 0; i < 12; i++ {
		readSSE(t, e.post(t, chatBody, nil))
	}
	mu.Lock()
	defer mu.Unlock()
	owner := map[string]string{}
	for tok, addrs := range remoteByToken {
		for a := range addrs {
			if prev, ok := owner[a]; ok && prev != tok {
				t.Fatalf("connection %s was used by two accounts (%s and %s)", a, prev, tok)
			}
			owner[a] = tok
		}
	}
	if len(remoteByToken) != 3 {
		t.Fatalf("expected 3 accounts to serve, got %d", len(remoteByToken))
	}
}
