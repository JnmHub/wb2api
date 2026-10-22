package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/store/accounts"
)

type failAfterReader struct {
	data []byte
	read bool
}

func (r *failAfterReader) Read(p []byte) (int, error) {
	if !r.read {
		r.read = true
		return copy(p, r.data), nil
	}
	return 0, errors.New("upstream read failed")
}

func testAccount() *pool.Account {
	return pool.New([]*accounts.Record{accounts.FromMap(map[string]any{"uid": "a"}, "", time.Now())}).All()[0]
}

func TestPipeStreamDoesNotSendDoneAfterUpstreamReadError(t *testing.T) {
	events := make(chan UsageEvent, 1)
	h := &Handler{OnUsage: func(ev UsageEvent) { events <- ev }}
	body := &failAfterReader{data: []byte("data: {\"id\":\"u\",\"choices\":[{\"delta\":{\"content\":\"part\"},\"finish_reason\":null}]}\n\n")}
	r := httptest.NewRequest("POST", "/", nil)
	w := httptest.NewRecorder()
	h.pipeStream(w, r, func() {}, time.Second, body, testAccount(), "m", "", "", time.Now(), 0)
	text := w.Body.String()
	if strings.Contains(text, "data: [DONE]") {
		t.Fatalf("unexpected success terminator after upstream error: %s", text)
	}
	if !strings.Contains(text, `"content":"part"`) || !strings.Contains(text, `"error":`) {
		t.Fatalf("partial output and explicit error must both survive: %s", text)
	}
	if strings.Contains(text, "upstream read failed") {
		t.Fatalf("raw upstream read error leaked: %s", text)
	}
	if ev := <-events; ev.Outcome != "upstream_aborted" {
		t.Fatalf("outcome = %q", ev.Outcome)
	}
}

func TestPipeStreamUpstreamErrorEnvelopeAbortsWithoutDone(t *testing.T) {
	events := make(chan UsageEvent, 1)
	h := &Handler{OnUsage: func(ev UsageEvent) { events <- ev }}
	body := strings.NewReader("data: {\"id\":\"u\",\"choices\":[{\"delta\":{\"content\":\"part\"}}]}\n\n" +
		"data: {\"error\":{\"message\":\"upstream failed\",\"type\":\"server_error\"}}\n\n")
	r := httptest.NewRequest("POST", "/", nil)
	w := httptest.NewRecorder()
	h.pipeStream(w, r, func() {}, time.Second, body, testAccount(), "m", "", "", time.Now(), 0)
	text := w.Body.String()
	if strings.Contains(text, "data: [DONE]") {
		t.Fatalf("unexpected success terminator after SSE error: %s", text)
	}
	if !strings.Contains(text, `"content":"part"`) || !strings.Contains(text, `"error":`) {
		t.Fatalf("buffered content and explicit error must both survive: %s", text)
	}
	if strings.Contains(text, "upstream failed") {
		t.Fatalf("raw upstream error leaked: %s", text)
	}
	if ev := <-events; ev.Outcome != "upstream_aborted" {
		t.Fatalf("outcome = %q", ev.Outcome)
	}
}

func TestAggregateClientCancellationDoesNotEmitUpstreamFailure(t *testing.T) {
	events := make(chan UsageEvent, 1)
	h := &Handler{OnUsage: func(ev UsageEvent) { events <- ev }}
	r := httptest.NewRequest("POST", "/", nil)
	ctx, cancel := context.WithCancel(r.Context())
	r = r.WithContext(ctx)
	cancel()
	w := httptest.NewRecorder()
	h.aggregate(w, r, func() {}, time.Second, strings.NewReader(""), testAccount(), "m", "", "", time.Now())
	ev := <-events
	if ev.Outcome != "client_aborted" {
		t.Fatalf("outcome = %q", ev.Outcome)
	}
	if ev.Status != http.StatusOK {
		t.Fatalf("status = %d", ev.Status)
	}
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("unexpected response after cancellation: status=%d body=%q", w.Code, w.Body.String())
	}
}
