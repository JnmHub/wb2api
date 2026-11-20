package openai

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"wb2api/internal/realm"
)

func TestCodeArtsInlineReasoning(t *testing.T) {
	const deepSeek = "deepseek-v4.1-flash"
	const literalXML = "答案🌏\n\n" + "```xml\n<thinking>示例</thinking>\n```\n<answer>保留🧩</answer>"

	for _, tc := range []struct {
		name, model          string
		deltas               []string
		finish               string
		content, reasoning   string
		empty, reasoningOnly bool
	}{
		{
			name:  "split tags and structured reasoning",
			model: deepSeek,
			deltas: []string{
				`{"role":"assistant","reasoning_content":"第一步💡"}`,
				`{"content":"<thi"}`, `{"content":"nking>"}`,
				`{"content":"进一步检索🔍"}`,
				`{"content":"</thin"}`, `{"content":"king>答案🌏"}`,
				fmt.Sprintf(`{"content":%q}`, strings.TrimPrefix(literalXML, "答案🌏")),
			},
			finish: "stop", content: literalXML, reasoning: "第一步💡进一步检索🔍",
		},
		{
			name: "adjacent reasoning tags on one line", model: deepSeek,
			deltas: []string{`{"content":"<thinking>甲🧪</thinking><thinking>乙🔬</thinking>可见答案"}`},
			finish: "stop", content: "可见答案", reasoning: "甲🧪乙🔬",
		},
		{
			name: "wrapped answer with split closing tag", model: deepSeek,
			deltas: []string{
				`{"content":"<assistant_response><thinking>确认🔍</thinking>"}`,
				`{"content":"完成✨</assistant_"}`, `{"content":"response>"}`,
			},
			finish: "stop", content: "完成✨", reasoning: "确认🔍",
		},
		{
			name: "only the terminal wrapper close is removed", model: deepSeek,
			deltas: []string{
				`{"content":"<assistant_response><thinking>先检查🔎</thinking>正文开始"}`,
				"{\"content\":\"\\n```xml\\n\"}",
				`{"content":"</assistant_response>"}`,
				"{\"content\":\"\\n```\\n继续正文✨\"}",
				`{"content":"</assistant_response>"}`,
			},
			finish: "stop", content: "正文开始\n```xml\n</assistant_response>\n```\n继续正文✨", reasoning: "先检查🔎",
		},
		{
			name: "bare assistant element stays literal", model: deepSeek,
			deltas: []string{
				`{"content":"<assistant_response>"}`,
				`{"content":"普通 XML✨</assistant_response>"}`,
			},
			finish: "stop", content: "<assistant_response>普通 XML✨</assistant_response>",
		},
		{
			name: "truncated reasoning keeps length error", model: deepSeek,
			deltas: []string{`{"content":"<thinking>这段推理"}`, `{"content":"尚未完成💭"}`},
			finish: "length", reasoning: "这段推理尚未完成💭", reasoningOnly: true,
		},
		{
			name: "truncated fullwidth DSML in reasoning stays out of the answer", model: deepSeek,
			deltas: []string{
				`{"content":"<thinking>准备查询🔍"}`,
				`{"content":"<｜｜DSML｜｜ calls><｜｜DSML｜｜ invo"}`,
			},
			finish: "length", reasoning: "准备查询🔍<｜｜DSML｜｜ calls><｜｜DSML｜｜ invo", reasoningOnly: true,
		},
		{
			name: "repeated empty envelopes are not an answer", model: deepSeek,
			deltas: []string{
				`{"content":"</thinking><assistant_response><thi"}`,
				`{"content":"nking> </thinking></assistant_response>"}`,
				`{"content":"<assistant_response><thinking> </thinking></assistant_response>"}`,
			},
			finish: "stop", empty: true,
		},
		{
			name: "other CodeArts models keep literal tags", model: "GLM-5.2",
			deltas: []string{`{"content":"<thinking>示例</thinking>答案"}`},
			finish: "stop", content: "<thinking>示例</thinking>答案",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, stream := range []bool{true, false} {
				t.Run(fmt.Sprintf("stream_%t", stream), func(t *testing.T) {
					e := newCodeartsEnv(t, func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "text/event-stream")
						for _, delta := range tc.deltas {
							fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":%s}]}\n\n", delta)
							w.(http.Flusher).Flush()
						}
						fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":%q}]}\n\n", tc.finish)
						fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":4,\"total_tokens\":12}}\n\n")
						fmt.Fprint(w, "data: [DONE]\n\n")
					})
					request := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"stream":%t,"stream_options":{"include_usage":true}}`, tc.model, stream)
					resp := e.post(t, request)
					defer resp.Body.Close()
					raw, err := io.ReadAll(resp.Body)
					if err != nil {
						t.Fatal(err)
					}
					wantStatus := http.StatusOK
					if tc.empty || (tc.reasoningOnly && !stream) {
						wantStatus = http.StatusBadGateway
					}
					if resp.StatusCode != wantStatus || !utf8.Valid(raw) {
						t.Fatalf("HTTP %d, want %d and valid UTF-8: %s", resp.StatusCode, wantStatus, raw)
					}
					got := readInlineReasoningReply(t, raw, stream && wantStatus == http.StatusOK)
					if tc.empty {
						if got.err == "" || got.content != "" || got.reasoning != "" || got.done {
							t.Fatalf("empty envelopes produced output: %s", raw)
						}
						return
					}
					if tc.reasoningOnly {
						wantReasoning := tc.reasoning
						if !stream {
							wantReasoning = ""
						}
						if !strings.Contains(got.err, "finish_reason=length") || got.done ||
							got.content != "" || got.reasoning != wantReasoning {
							t.Fatalf("truncated reasoning lost its budget error or leaked into text: %s", raw)
						}
						return
					}
					if got.content != tc.content || got.reasoning != tc.reasoning || got.finish != tc.finish ||
						got.err != "" || got.done != stream || got.totalTokens != 12 {
						t.Fatalf("unexpected Chat Completions fields: %s", raw)
					}
				})
			}
		})
	}
}

func TestCodeArtsInlineReasoningInCompatibilityStreams(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	for _, tc := range []struct {
		name, payload, terminal string
		handler                 func(*Handler) http.Handler
	}{
		{
			name: "messages", terminal: "message_stop",
			payload: `{"model":"deepseek-v4.1-flash","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`,
			handler: func(h *Handler) http.Handler { return &Messages{Chat: h} },
		},
		{
			name: "responses", terminal: "response.completed",
			payload: `{"model":"deepseek-v4.1-flash","stream":true,"input":"hi"}`,
			handler: func(h *Handler) http.Handler { return &Responses{Chat: h} },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newCodeartsEnv(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"<think\"}}]}\n\n")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ing>思考💡</thinking>答复🌏\"}}]}\n\n")
				fmt.Fprint(w, finishChunk, doneChunk)
			})
			r := httptest.NewRequest(http.MethodPost, "/v1/"+tc.name, strings.NewReader(tc.payload))
			r.Header.Set("X-Realm", realm.CodeArts)
			w := httptest.NewRecorder()
			tc.handler(e.h).ServeHTTP(w, r)
			raw := w.Body.String()
			var text, reasoning string
			terminal := false
			for _, line := range strings.Split(raw, "\n") {
				payload, ok := strings.CutPrefix(line, "data: ")
				if !ok || payload == "[DONE]" {
					continue
				}
				var event struct {
					Type  string
					Delta json.RawMessage
				}
				if err := json.Unmarshal([]byte(payload), &event); err != nil {
					t.Fatal(err)
				}
				switch event.Type {
				case "content_block_delta":
					var delta struct{ Type, Thinking, Text string }
					if err := json.Unmarshal(event.Delta, &delta); err != nil {
						t.Fatal(err)
					}
					if delta.Type == "thinking_delta" {
						reasoning += delta.Thinking
					} else if delta.Type == "text_delta" {
						text += delta.Text
					}
				case "response.reasoning_summary_text.delta", "response.output_text.delta":
					var delta string
					if err := json.Unmarshal(event.Delta, &delta); err != nil {
						t.Fatal(err)
					}
					if event.Type == "response.reasoning_summary_text.delta" {
						reasoning += delta
					} else {
						text += delta
					}
				case tc.terminal:
					terminal = true
				}
			}
			if w.Code != http.StatusOK || text != "答复🌏" || reasoning != "思考💡" || !terminal {
				t.Fatalf("unexpected %s stream status %d: %s", tc.name, w.Code, raw)
			}
		})
		t.Run(tc.name+" rejects empty envelopes", func(t *testing.T) {
			e := newCodeartsEnv(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for _, text := range []string{assistantOpen, thinkingOpen, " ", thinkingClose, assistantClose} {
					encoded, _ := json.Marshal(text)
					fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", encoded)
				}
				fmt.Fprint(w, finishChunk, doneChunk)
			})
			r := httptest.NewRequest(http.MethodPost, "/v1/"+tc.name, strings.NewReader(tc.payload))
			r.Header.Set("X-Realm", realm.CodeArts)
			w := httptest.NewRecorder()
			tc.handler(e.h).ServeHTTP(w, r)
			if w.Code != http.StatusBadGateway {
				t.Fatalf("%s returned HTTP %d for empty envelopes: %s", tc.name, w.Code, w.Body)
			}
		})
	}
}

type inlineReasoningReply struct {
	content, reasoning, finish, err string
	done                            bool
	totalTokens                     int
}

func readInlineReasoningReply(t *testing.T, raw []byte, stream bool) inlineReasoningReply {
	t.Helper()
	frames := [][]byte{raw}
	if stream {
		frames = chatSSEPayloads(t, raw)
	}
	var out inlineReasoningReply
	for _, payload := range frames {
		if string(payload) == "[DONE]" {
			out.done = true
			continue
		}
		type assistant struct {
			Content   string `json:"content"`
			Reasoning string `json:"reasoning_content"`
		}
		var event struct {
			Choices []struct {
				Delta, Message assistant
				Finish         string `json:"finish_reason"`
			}
			Error *struct {
				Message string `json:"message"`
			}
			Usage *struct {
				TotalTokens int `json:"total_tokens"`
			}
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatal(err)
		}
		for _, choice := range event.Choices {
			out.content += choice.Delta.Content + choice.Message.Content
			out.reasoning += choice.Delta.Reasoning + choice.Message.Reasoning
			if choice.Finish != "" {
				out.finish = choice.Finish
			}
		}
		if event.Error != nil {
			out.err = event.Error.Message
		}
		if event.Usage != nil {
			out.totalTokens = event.Usage.TotalTokens
		}
	}
	return out
}
