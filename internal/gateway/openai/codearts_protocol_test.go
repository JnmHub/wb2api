package openai

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wb2api/internal/realm"
)

func chatSSEPayloads(t *testing.T, raw []byte) [][]byte {
	t.Helper()
	var out [][]byte
	for _, event := range bytes.Split(raw, []byte("\n\n")) {
		data, ok := bytes.CutPrefix(event, []byte("data: "))
		if !ok {
			continue
		}
		if !bytes.Equal(data, []byte("[DONE]")) && !json.Valid(data) {
			t.Fatalf("invalid downstream SSE JSON %q", data)
		}
		out = append(out, data)
	}
	return out
}

func TestCodeArtsChatStreamFramingAndUsage(t *testing.T) {
	for _, tc := range []struct {
		name, option string
		include      bool
	}{
		{"omitted", "", false},
		{"false", `,"stream_options":{"include_usage":false}`, false},
		{"true", `,"stream_options":{"include_usage":true}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newCodeartsEnv(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Content-Encoding", "gzip")
				zw := gzip.NewWriter(w)
				defer zw.Close()
				fmt.Fprint(zw, "\ufeff: ping\r\nevent: message\r\n")
				fmt.Fprint(zw, "data: {\"id\":\"c-1\",\"created\":42,\r\ndata: \"choices\":[{\"delta\":{\"role\":\"assistant\",\"reasoning\":\"检索🧪\"}}]}\r\n\r\n")
				fmt.Fprint(zw, "data: {\"choices\":[{\"delta\":\r\ndata: {\"content\":\"你好🌐\"}}]}\r\n\r\n")
				fmt.Fprint(zw, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":2,\"total_tokens\":10}}\r\n\r\n")
				fmt.Fprint(zw, "data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\r\n\r\ndata: [DONE]\r\n\r\n")
			})
			resp := e.post(t, `{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],"stream":true`+tc.option+`}`)
			raw, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("HTTP %d %s (%v)", resp.StatusCode, raw, err)
			}
			data := chatSSEPayloads(t, raw)
			wantFrames := 4
			if tc.include {
				wantFrames++
			}
			if len(data) != wantFrames || string(data[len(data)-1]) != "[DONE]" {
				t.Fatalf("unexpected frames: %s", raw)
			}
			if !bytes.Contains(data[0], []byte(`"reasoning_content":"检索🧪"`)) || !bytes.Contains(data[1], []byte(`"content":"你好🌐"`)) || !bytes.Contains(data[2], []byte(`"finish_reason":"stop"`)) {
				t.Fatalf("reasoning/content/finish order: %s", raw)
			}
			if tc.include {
				for _, frame := range data[:3] {
					if !bytes.Contains(frame, []byte(`"usage":null`)) || !bytes.Contains(frame, []byte(`"id":"c-1"`)) {
						t.Fatalf("ordinary chunk metadata: %s", frame)
					}
				}
				if !bytes.Contains(data[3], []byte(`"choices":[]`)) || !bytes.Contains(data[3], []byte(`"total_tokens":10`)) {
					t.Fatalf("final usage chunk: %s", data[3])
				}
			} else if bytes.Contains(raw, []byte(`"usage"`)) {
				t.Fatalf("unrequested usage leaked: %s", raw)
			}
			ev := <-e.events
			if ev.Outcome != "completed" || usageTotal(ev.Usage) != 10 || ev.Estimated {
				t.Fatalf("usage event: %+v", ev)
			}
		})
	}
}

func TestCodeArtsChatStreamFaultBeforeAndAfterCommit(t *testing.T) {
	for _, tc := range []struct {
		name, beforeFault string
		stream, plaintext bool
		status            int
	}{
		{"SSE before commit", "", true, false, http.StatusPaymentRequired},
		{"JSON before commit", "", false, true, http.StatusPaymentRequired},
		{"SSE after commit", `data: {"choices":[{"delta":{"content":"你好"}}]}` + "\n\n", true, false, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newCodeartsEnv(t, func(w http.ResponseWriter, r *http.Request) {
				body := `{"error_code":"InferHub.4291.200","error_msg":"insufficient quota","details":[{"token":"private-upstream-detail"}]}`
				if tc.plaintext {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, body)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.beforeFault, "data: ", body, "\n\ndata: [DONE]\n\n")
			})
			if tc.beforeFault != "" {
				second := *e.h.Pool.Get("hw-1").Record()
				second.UID = "hw-2"
				e.h.Pool.Add(&second)
			}
			resp := e.post(t, fmt.Sprintf(`{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],"stream":%t}`, tc.stream))
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tc.status || bytes.Contains(raw, []byte("[DONE]")) || bytes.Contains(raw, []byte("private-upstream-detail")) || !bytes.Contains(raw, []byte("InferHub.4291.200")) {
				t.Fatalf("HTTP %d %s (expected %d)", resp.StatusCode, raw, tc.status)
			}
			if tc.beforeFault != "" {
				frames := chatSSEPayloads(t, raw)
				if len(frames) != 2 || !bytes.Contains(frames[0], []byte("你好")) || !bytes.Contains(frames[1], []byte(`"error"`)) {
					t.Fatalf("expected partial content followed by one error: %s", raw)
				}
			}
			if ev := <-e.events; ev.Outcome == "completed" || ev.UpstreamCode != "InferHub.4291.200" || e.hits.Load() != 1 {
				t.Fatalf("usage event: %+v, upstream hits %d", ev, e.hits.Load())
			}
		})
	}
}

func TestCodeArtsSSEFramingInCompatibilityEndpoints(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	for _, tc := range []struct {
		path, payload, terminal string
		serve                   func(*Handler) http.Handler
	}{
		{"/v1/responses", `{"model":"GLM-5.2","stream":true,"input":"hi"}`, "event: response.completed", func(h *Handler) http.Handler { return &Responses{Chat: h} }},
		{"/v1/messages", `{"model":"GLM-5.2","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, "event: message_stop", func(h *Handler) http.Handler { return &Messages{Chat: h} }},
	} {
		t.Run(tc.path, func(t *testing.T) {
			e := newCodeartsEnv(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":\r\ndata: {\"content\":\"你好🌐\"}}]}\r\n\r\n")
				fmt.Fprint(w, "data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\r\n\r\ndata: [DONE]\r\n\r\n")
			})
			r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.payload))
			r.Header.Set("X-Realm", realm.CodeArts)
			w := httptest.NewRecorder()
			tc.serve(e.h).ServeHTTP(w, r)
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "你好🌐") || !strings.Contains(w.Body.String(), tc.terminal) {
				t.Fatalf("HTTP %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestCodeArtsChatMalformedUTF8AbortsAfterContent(t *testing.T) {
	e := newCodeartsEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"你好"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"`+string([]byte{0xff})+`"}}]}`+"\n\n")
	})
	resp := e.post(t, `{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "你好") || strings.Contains(string(raw), "�") || strings.Contains(string(raw), "[DONE]") || !strings.Contains(string(raw), `"error"`) {
		t.Fatalf("invalid UTF-8 was not treated as a post-commit error: %s", raw)
	}
}

func TestCodeArtsChatNamedToolAcrossStreamingAndAggregate(t *testing.T) {
	long := strings.Repeat("lookup_service_", 6)
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream_%t", stream), func(t *testing.T) {
			var e *codeartsEnv
			e = newCodeartsEnv(t, func(w http.ResponseWriter, r *http.Request) {
				var wire struct {
					Choice   string `json:"tool_choice"`
					Tools    []struct{ Function struct{ Name string } }
					Messages []struct {
						Role       string
						Content    string
						ToolCallID string                                     `json:"tool_call_id"`
						ToolCalls  []struct{ Function struct{ Name string } } `json:"tool_calls"`
					}
				}
				if err := json.Unmarshal(e.body, &wire); err != nil || wire.Choice != "required" || len(wire.Tools) != 1 || len(wire.Messages) != 3 || len(wire.Messages[0].ToolCalls) != 1 {
					t.Errorf("named selection/history was corrupted: %s (%v)", e.body, err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				name := wire.Tools[0].Function.Name
				if len(name) != 64 || wire.Messages[0].ToolCalls[0].Function.Name != name || wire.Messages[1].Role != "tool" || wire.Messages[1].ToolCallID != "previous" || wire.Messages[1].Content != "结果：🧪" {
					t.Errorf("incorrect tool history: %s", e.body)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-now","type":"function","function":{"name":%q,"arguments":""}}]}}]}`+"\n\n", name[:24])
				fmt.Fprintf(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":%q,"arguments":%q}}]}}]}`+"\n\n", name[24:], `{"city":"`)
				fmt.Fprintf(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]}}]}`+"\n\n", `深圳"}`)
				fmt.Fprint(w, `data: {"choices":[{"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
			})
			request := fmt.Sprintf(`{"model":"GLM-5.2","stream":%t,"messages":[
				{"role":"assistant","content":null,"tool_calls":[{"id":"previous","type":"function","function":{"name":%q,"arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"previous","content":"结果：🧪"},
				{"role":"user","content":"next"}],"tools":[{"type":"function","function":{"name":"other"}},
				{"type":"function","function":{"name":%q}}],"tool_choice":{"type":"function","function":{"name":%q}}}`, stream, long, long, long)
			resp := e.post(t, request)
			raw, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("HTTP %d %s (%v)", resp.StatusCode, raw, err)
			}
			var frames [][]byte
			if stream {
				frames = chatSSEPayloads(t, raw)
				if len(frames) < 2 || string(frames[len(frames)-1]) != "[DONE]" {
					t.Fatalf("unexpected SSE: %s", raw)
				}
				frames = frames[:len(frames)-1]
			} else {
				frames = [][]byte{raw}
			}
			var arguments, names, finish, id string
			for _, frame := range frames {
				type assistantPart struct {
					ToolCalls []struct {
						ID       string
						Function struct{ Name, Arguments string }
					} `json:"tool_calls"`
				}
				var event struct {
					Choices []struct {
						Finish         string `json:"finish_reason"`
						Delta, Message assistantPart
					}
				}
				if err := json.Unmarshal(frame, &event); err != nil {
					t.Fatal(err)
				}
				for _, ch := range event.Choices {
					if ch.Finish != "" {
						finish = ch.Finish
					}
					for _, call := range append(ch.Delta.ToolCalls, ch.Message.ToolCalls...) {
						if call.ID != "" {
							id = call.ID
						}
						arguments += call.Function.Arguments
						names += call.Function.Name
					}
				}
			}
			if id != "call-now" || names != long || arguments != `{"city":"深圳"}` || finish != "tool_calls" {
				t.Fatalf("tool-call metadata/arguments: %s", raw)
			}
		})
	}
}
