package openai

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	reasoningChunk = "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n"
	contentChunk   = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n"
	finishChunk    = "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	doneChunk      = "data: [DONE]\n\n"
)

// A reply that only thinks used to be forwarded as a successful empty answer.
// It must now end as a protocol-level error on all three surfaces.
func TestReasoningOnlyReplyFailsAcrossProtocols(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			w := &relayTestWriter{header: http.Header{}}
			body := reasoningChunk + finishChunk + doneChunk
			runRelayProtocol(protocol, &Handler{}, w, strings.NewReader(body), time.Second, func() {}, 0)
			out := w.body.String()
			if !strings.Contains(out, reasoningOnlyDetail) {
				t.Fatalf("no failure notice in output: %s", out)
			}
			for _, ok := range []string{`[DONE]`, `response.completed`, `message_stop`} {
				if strings.Contains(out, ok) {
					t.Fatalf("reasoning-only reply still reported success (%s): %s", ok, out)
				}
			}
		})
	}
}

func TestContentReplyStillCompletes(t *testing.T) {
	w := &relayTestWriter{header: http.Header{}}
	body := contentChunk + finishChunk + doneChunk
	runRelayProtocol("chat", &Handler{}, w, strings.NewReader(body), time.Second, func() {}, 0)
	out := w.body.String()
	if strings.Contains(out, reasoningOnlyDetail) {
		t.Fatalf("content reply flagged as reasoning-only: %s", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("content reply got no [DONE]: %s", out)
	}
}

func TestToolCallOnlyReplyIsNotReasoningOnly(t *testing.T) {
	w := &relayTestWriter{header: http.Header{}}
	body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"plan\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		finishChunk + doneChunk
	runRelayProtocol("chat", &Handler{}, w, strings.NewReader(body), time.Second, func() {}, 0)
	out := w.body.String()
	if strings.Contains(out, reasoningOnlyDetail) {
		t.Fatalf("tool call reply flagged as reasoning-only: %s", out)
	}
}
