package openai

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wb2api/internal/catalog"
)

// respEnv is the chat test environment plus a /v1/responses route.
func respEnv(t *testing.T, up http.Handler, settingsJSON string) (*env, *httptest.Server) {
	t.Helper()
	e := newEnv(t, up, 2, settingsJSON)
	e.h.Catalog = &catalog.Service{}
	srv := httptest.NewServer(&Responses{Chat: e.h})
	t.Cleanup(srv.Close)
	return e, srv
}

func postJSON(t *testing.T, url, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
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

type sseEvent struct {
	name string
	data map[string]any
}

func readEvents(t *testing.T, resp *http.Response) []sseEvent {
	t.Helper()
	defer resp.Body.Close()
	var out []sseEvent
	var name string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			raw := strings.TrimPrefix(line, "data: ")
			if raw == "[DONE]" {
				out = append(out, sseEvent{name: "[DONE]"})
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(raw), &m); err != nil {
				t.Fatalf("event %s carries invalid JSON %q", name, raw)
			}
			if m["type"] != name {
				t.Fatalf("event name %q but data type %v", name, m["type"])
			}
			out = append(out, sseEvent{name: name, data: m})
		}
	}
	return out
}

func names(evs []sseEvent) string {
	var n []string
	for _, e := range evs {
		n = append(n, e.name)
	}
	return strings.Join(n, ",")
}

func completed(t *testing.T, evs []sseEvent) map[string]any {
	t.Helper()
	last := evs[len(evs)-1]
	if last.name != "response.completed" {
		t.Fatalf("stream ends with %s: %s", last.name, names(evs))
	}
	return last.data["response"].(map[string]any)
}

func TestResponsesStreamText(t *testing.T) {
	var seen struct {
		sync.Mutex
		body string
	}
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen.Lock()
		seen.body = string(b)
		seen.Unlock()
		sseUpstream("Hel", "lo")(w, r)
	})
	e, srv := respEnv(t, up, "")
	resp := postJSON(t, srv.URL, `{"model":"glm-5.3","stream":true,"instructions":"be brief","input":"hi","max_output_tokens":50}`, nil)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	evs := readEvents(t, resp)
	if !strings.HasPrefix(names(evs), "response.created,response.in_progress,response.output_item.added,response.content_part.added,response.output_text.delta") {
		t.Fatalf("events: %s", names(evs))
	}
	final := completed(t, evs)
	if final["output_text"] != "Hello" || final["status"] != "completed" || final["model"] != "glm-5.3" {
		t.Errorf("%v", final)
	}
	usage := final["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 10 || usage["output_tokens"].(float64) != 5 || usage["total_tokens"].(float64) != 15 {
		t.Errorf("usage %v", usage)
	}
	// what the upstream saw is an ordinary chat request
	seen.Lock()
	defer seen.Unlock()
	for _, want := range []string{`"role":"system"`, `be brief`, `"max_tokens":50`, `"stream":true`, `"role":"user"`} {
		if !strings.Contains(seen.body, want) {
			t.Errorf("upstream body lacks %s: %s", want, seen.body)
		}
	}
	ev := e.event(t)
	if ev.Outcome != "completed" || !ev.Stream || ev.Model != "glm-5.3" || ev.Account == "" || string(ev.Usage) == "" {
		t.Errorf("usage event %+v", ev)
	}
	// the sequence numbers count up from 1 without gaps
	for i, e := range evs {
		if int(e.data["sequence_number"].(float64)) != i+1 {
			t.Fatalf("sequence_number %v at position %d", e.data["sequence_number"], i)
		}
	}
}

func TestResponsesStreamToolCallAndCustomTool(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"id":"u","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_weather","arguments":""}}]}}]}`+"\n\n")
		io.WriteString(w, `data: {"id":"u","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`+"\n\n")
		io.WriteString(w, `data: {"id":"u","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]}}]}`+"\n\n")
		io.WriteString(w, `data: {"id":"u","choices":[{"index":1,"delta":{"tool_calls":[{"index":1,"id":"call_2","function":{"name":"apply_patch","arguments":"{\"input\":\"*** Begin\"}"}}]}}]}`+"\n\n")
		io.WriteString(w, `data: {"id":"u","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		io.WriteString(w, `data: {"id":"u","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`+"\n\ndata: [DONE]\n\n")
	})
	_, srv := respEnv(t, up, "")
	body := `{"model":"glm-5.3","stream":true,"input":"go","tools":[
	  {"type":"function","name":"get_weather","parameters":{"type":"object"}},
	  {"type":"custom","name":"apply_patch","format":{"type":"grammar","definition":"x"}}]}`
	evs := readEvents(t, postJSON(t, srv.URL, body, nil))
	final := completed(t, evs)
	out := final["output"].([]any)
	if len(out) != 2 {
		t.Fatalf("output %v (events %s)", out, names(evs))
	}
	fc := out[0].(map[string]any)
	if fc["type"] != "function_call" || fc["name"] != "get_weather" || fc["call_id"] != "call_1" || fc["arguments"] != `{"city":"Paris"}` {
		t.Errorf("function call %v", fc)
	}
	ctc := out[1].(map[string]any)
	if ctc["type"] != "custom_tool_call" || ctc["name"] != "apply_patch" || ctc["input"] != "*** Begin" {
		t.Errorf("custom tool call must be re-inflated with the raw payload: %v", ctc)
	}
	// the freeform tool was declared upstream as an ordinary function
	if !strings.Contains(names(evs), "response.custom_tool_call_input.done") || !strings.Contains(names(evs), "response.function_call_arguments.done") {
		t.Errorf("events %s", names(evs))
	}
}

func TestResponsesNonStream(t *testing.T) {
	e, srv := respEnv(t, sseUpstream("Hello ", "world"), "")
	resp := postJSON(t, srv.URL, `{"model":"glm-5.3","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"tool_choice":"auto"}`, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if out["object"] != "response" || out["status"] != "completed" || out["output_text"] != "Hello world" || out["tool_choice"] != "auto" {
		t.Errorf("%s", body)
	}
	if !strings.HasPrefix(out["id"].(string), "resp_") {
		t.Errorf("id %v", out["id"])
	}
	msg := out["output"].([]any)[0].(map[string]any)
	if msg["type"] != "message" || msg["role"] != "assistant" {
		t.Errorf("%v", msg)
	}
	if ev := e.event(t); ev.Outcome != "completed" || ev.Stream {
		t.Errorf("usage event %+v", ev)
	}
}

func TestResponsesRejectsPreviousResponseID(t *testing.T) {
	_, srv := respEnv(t, sseUpstream("x"), "")
	resp := postJSON(t, srv.URL, `{"model":"glm-5.3","input":"hi","previous_response_id":"resp_1"}`, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(body), "previous_response_id") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func TestResponsesAuthAndMethod(t *testing.T) {
	_, srv := respEnv(t, sseUpstream("x"), `{"api_keys":[{"id":"k","key":"sk-1","enabled":true}]}`)
	if resp := postJSON(t, srv.URL, `{"model":"glm-5.3","input":"hi"}`, nil); resp.StatusCode != 401 {
		t.Errorf("no key: %d", resp.StatusCode)
	}
	if resp := postJSON(t, srv.URL, `{"model":"glm-5.3","input":"hi"}`, map[string]string{"Authorization": "Bearer sk-1"}); resp.StatusCode != 200 {
		t.Errorf("good key: %d", resp.StatusCode)
	}
	resp, err := http.Get(srv.URL)
	if err != nil || resp.StatusCode != 405 {
		t.Errorf("GET: %v %v", resp, err)
	}
	if resp := postJSON(t, srv.URL, `[1]`, map[string]string{"Authorization": "Bearer sk-1"}); resp.StatusCode != 400 {
		t.Errorf("non-object body: %d", resp.StatusCode)
	}
}

func TestResponsesUpstreamErrorsUseTheChatPipeline(t *testing.T) {
	// the first account is rate limited, the second answers: failover is shared
	var mu sync.Mutex
	n := 0
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		if first {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		sseUpstream("ok")(w, r)
	})
	_, srv := respEnv(t, up, "")
	evs := readEvents(t, postJSON(t, srv.URL, `{"model":"glm-5.3","stream":true,"input":"hi"}`, nil))
	if completed(t, evs)["output_text"] != "ok" {
		t.Fatal(names(evs))
	}

	dead := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusPaymentRequired) })
	_, srv2 := respEnv(t, dead, "")
	resp := postJSON(t, srv2.URL, `{"model":"glm-5.3","input":"hi"}`, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 400 || !strings.Contains(string(body), `"error"`) {
		t.Errorf("exhausted accounts must be an error, got %d %s", resp.StatusCode, body)
	}
}

func TestResponsesBrokenUpstreamIsNotReportedAsCompleted(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"id":"u","choices":[{"index":0,"delta":{"content":"par"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(50 * time.Millisecond)
		panic(http.ErrAbortHandler) // drop the connection mid-stream
	})
	e, srv := respEnv(t, up, "")
	evs := readEvents(t, postJSON(t, srv.URL, `{"model":"glm-5.3","stream":true,"input":"hi"}`, nil))
	for _, ev := range evs {
		if ev.name == "response.completed" {
			t.Fatalf("a cut stream must not end with response.completed: %s", names(evs))
		}
	}
	if ev := e.event(t); ev.Outcome != "upstream_aborted" {
		t.Errorf("outcome %q", ev.Outcome)
	}
}
