package openai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wb2api/internal/catalog"
)

func msgEnv(t *testing.T, up http.Handler, settingsJSON string) (*env, *httptest.Server) {
	t.Helper()
	e := newEnv(t, up, 2, settingsJSON)
	e.h.Catalog = &catalog.Service{}
	srv := httptest.NewServer(&Messages{Chat: e.h})
	t.Cleanup(srv.Close)
	return e, srv
}

const msgBody = `{"model":"glm-5.3","max_tokens":64,"messages":[{"role":"user","content":"hi"}]%s}`

func TestMessagesDisabledByDefault(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "")
	_, srv := msgEnv(t, sseUpstream("x"), "")
	resp := postJSON(t, srv.URL, strings.Replace(msgBody, "%s", "", 1), nil)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(b), `"type":"error"`) || !strings.Contains(string(b), "disabled by default") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

func TestMessagesNonStream(t *testing.T) {
	_, srv := msgEnv(t, sseUpstream("Hello ", "there"), `{"enable_anthropic_messages":true}`)
	resp := postJSON(t, srv.URL, strings.Replace(msgBody, "%s", "", 1), nil)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	content := out["content"].([]any)
	if out["type"] != "message" || out["role"] != "assistant" || out["stop_reason"] != "end_turn" ||
		content[0].(map[string]any)["text"] != "Hello there" {
		t.Fatalf("%s", b)
	}
	if u := out["usage"].(map[string]any); u["input_tokens"].(float64) != 10 || u["output_tokens"].(float64) != 5 {
		t.Errorf("usage %v", u)
	}
}

func TestMessagesStream(t *testing.T) {
	e, srv := msgEnv(t, sseUpstream("a", "b"), `{"enable_anthropic_messages":true}`)
	resp := postJSON(t, srv.URL, strings.Replace(msgBody, "%s", `,"stream":true`, 1), nil)
	var names []string
	var text strings.Builder
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, block := range strings.Split(string(b), "\n\n") {
		var name, data string
		for _, l := range strings.Split(block, "\n") {
			if strings.HasPrefix(l, "event: ") {
				name = strings.TrimPrefix(l, "event: ")
			}
			if strings.HasPrefix(l, "data: ") {
				data = strings.TrimPrefix(l, "data: ")
			}
		}
		if name == "" {
			continue
		}
		names = append(names, name)
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil || m["type"] != name {
			t.Fatalf("event %s has bad data %q", name, data)
		}
		if name == "content_block_delta" {
			text.WriteString(m["delta"].(map[string]any)["text"].(string))
		}
	}
	want := "message_start,content_block_start,content_block_delta,content_block_delta,content_block_stop,message_delta,message_stop"
	if strings.Join(names, ",") != want || text.String() != "ab" {
		t.Fatalf("events %v text %q", names, text.String())
	}
	if ev := e.event(t); ev.Outcome != "completed" || !ev.Stream {
		t.Errorf("%+v", ev)
	}
}

func TestMessagesErrorsUseTheAnthropicEnvelope(t *testing.T) {
	_, srv := msgEnv(t, sseUpstream("x"), `{"enable_anthropic_messages":true,"api_keys":[{"id":"k","key":"sk-1","enabled":true}]}`)
	resp := postJSON(t, srv.URL, strings.Replace(msgBody, "%s", "", 1), nil)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 401 || !strings.Contains(string(b), "authentication_error") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	hdr := map[string]string{"x-api-key": "sk-1"}
	resp = postJSON(t, srv.URL, `{"model":"glm-5.3","messages":[{"role":"tool","content":"x"}]}`, hdr)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(b), "invalid_request_error") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

func TestMessagesCountTokens(t *testing.T) {
	_, srv := msgEnv(t, sseUpstream("x"), `{"enable_anthropic_messages":true}`)
	resp := postJSON(t, srv.URL+"/v1/messages/count_tokens", strings.Replace(msgBody, "%s", "", 1), nil)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var out map[string]float64
	if resp.StatusCode != 200 || json.Unmarshal(b, &out) != nil || out["input_tokens"] < 1 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

func TestMessagesRateLimitEnvelope(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"msg":"usage exceeds frequency limit"}`)
	})
	_, srv := msgEnv(t, up, `{"enable_anthropic_messages":true}`)
	resp := postJSON(t, srv.URL, strings.Replace(msgBody, "%s", "", 1), nil)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" || !strings.Contains(string(b), `"rate_limit_error"`) || !strings.Contains(string(b), `"type":"error"`) {
		t.Fatalf("%d %v %s", resp.StatusCode, resp.Header, b)
	}
}
