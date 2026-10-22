package openai

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wb2api/internal/gateway/responses"
	"wb2api/internal/jsonx"
	"wb2api/internal/webtools"
)

func TestMessagesErrorEnvelopeStopsOpenUpstream(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	for _, kind := range []string{"object", "string"} {
		t.Run(kind, func(t *testing.T) {
			errorData := `{"error":"secret upstream token"}`
			if kind == "object" {
				errorData = `{"error":{"message":"secret upstream token"}}`
			}
			closed := make(chan error, 1)
			up := newRawUpstream(t, func(_ int, _ rawReq, c net.Conn) {
				// Deliver content and error together, then leave the body unfinished.
				fmt.Fprint(c, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 99999\r\n\r\n"+
					responsePartialSSE+"data: "+errorData+"\n\n")
				_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
				var b [1]byte
				_, err := c.Read(b[:])
				closed <- err
			})
			f := newFixture(t, up, accSpec{})
			events := make(chan UsageEvent, 1)
			f.h.OnUsage = func(ev UsageEvent) { events <- ev }
			// Exercise replayed staging bytes as well as the live wire body.
			f.h.FirstOutputTimeout = 3 * time.Second
			srv := httptest.NewServer(&Messages{Chat: f.h})
			defer srv.Close()
			req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(strings.Replace(msgBody, "%s", `,"stream":true`, 1)))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			start := time.Now()
			resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("gateway kept reading after terminal envelope: %v", err)
			}
			if elapsed := time.Since(start); elapsed > 3*time.Second {
				t.Errorf("error waited for EOF/idle instead of ending immediately: %v", elapsed)
			}
			text := string(b)
			if resp.StatusCode != http.StatusOK || !strings.Contains(text, `"text":"part"`) || strings.Count(text, "event: error\n") != 1 {
				t.Errorf("partial content/error missing: status=%d body=%s", resp.StatusCode, text)
			}
			for _, forbidden := range []string{"message_stop", "data: [DONE]", "secret upstream token"} {
				if strings.Contains(text, forbidden) {
					t.Errorf("stream contains %q: %s", forbidden, text)
				}
			}
			select {
			case err := <-closed:
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					t.Fatalf("upstream body was not closed: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("upstream connection not released")
			}
			if ev := <-events; ev.Outcome != "failed" || ev.Status != http.StatusBadGateway {
				t.Errorf("usage = %+v", ev)
			}
			if n := f.pool.All()[0].Inflight(); n != 0 {
				t.Errorf("terminal error retained %d account slots", n)
			}
		})
	}
}

func TestAggregateProtocolErrorsAreSanitized(t *testing.T) {
	for _, protocol := range []string{"chat", "messages", "responses"} {
		for _, kind := range []string{"read", "object", "string", "canceled"} {
			t.Run(protocol+"/"+kind, func(t *testing.T) {
				var body io.Reader = &failAfterReader{data: []byte(responsePartialSSE)}
				switch kind {
				case "object":
					body = strings.NewReader(responsePartialSSE + "data: {\"error\":{\"message\":\"secret upstream token\"}}\n\n")
				case "string":
					body = strings.NewReader(responsePartialSSE + "data: {\"error\":\"secret upstream token\"}\n\n")
				}
				r := httptest.NewRequest(http.MethodPost, "/", nil)
				if kind == "canceled" {
					ctx, cancel := context.WithCancel(r.Context())
					cancel()
					r = r.WithContext(ctx)
				}
				events := make(chan UsageEvent, 1)
				h := &Handler{OnUsage: func(ev UsageEvent) { events <- ev }}
				w := httptest.NewRecorder()
				q := &request{r: r, w: w, payload: jsonx.NewObj(), start: time.Now()}
				switch protocol {
				case "chat":
					h.aggregate(w, r, func() {}, time.Minute, body, testAccount(), "m", "", "", q.start)
				case "messages":
					h.messagesWhole(q, testAccount(), func() {}, time.Minute, body, "m")
				case "responses":
					h.responsesWhole(q, testAccount(), func() {}, time.Minute, body, "m", responses.Info{})
				}
				ev := <-events
				if kind == "canceled" {
					if ev.Outcome != "client_aborted" || ev.Status != http.StatusOK || w.Body.Len() != 0 || w.Code != http.StatusOK {
						t.Fatalf("cancellation changed: usage=%+v status=%d body=%s", ev, w.Code, w.Body.String())
					}
					return
				}
				text := w.Body.String()
				if w.Code != http.StatusBadGateway || !strings.Contains(text, "upstream stream aborted") || !strings.Contains(text, `"error":`) {
					t.Errorf("wrong error envelope: status=%d body=%s", w.Code, text)
				}
				if strings.Contains(text, "secret upstream token") || strings.Contains(text, "upstream read failed") {
					t.Errorf("raw upstream error leaked: %s", text)
				}
				if protocol == "messages" && (!strings.Contains(text, `"type":"error"`) || !strings.Contains(text, `"type":"api_error"`)) {
					t.Errorf("wrong Anthropic envelope: %s", text)
				}
				if ev.Outcome != "upstream_aborted" || ev.Status != http.StatusBadGateway {
					t.Errorf("usage = %+v", ev)
				}
			})
		}
	}
}

func TestMessagesReadErrorIsSanitized(t *testing.T) {
	events := make(chan UsageEvent, 1)
	h := &Handler{OnUsage: func(ev UsageEvent) { events <- ev }}
	w := httptest.NewRecorder()
	q := &request{r: httptest.NewRequest(http.MethodPost, "/", nil), w: w, start: time.Now()}
	h.messagesStream(q, testAccount(), func() {}, time.Minute, &failAfterReader{data: []byte(responsePartialSSE)}, "m")
	text := w.Body.String()
	if !strings.Contains(text, `"text":"part"`) || !strings.Contains(text, "event: error") ||
		strings.Contains(text, "message_stop") || strings.Contains(text, "upstream read failed") {
		t.Errorf("bad failure stream: %s", text)
	}
	if ev := <-events; ev.Outcome != "upstream_aborted" || ev.Status != http.StatusBadGateway {
		t.Errorf("usage = %+v", ev)
	}
}

func TestResponsesFollowUpErrorsAreSanitized(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			up := newRawUpstream(t, func(_ int, _ rawReq, c net.Conn) {
				status(c, http.StatusBadGateway, `{"error":{"message":"secret upstream token"}}`)
			})
			f := newFixture(t, up, accSpec{})
			webtools.ExecuteHook = func(context.Context, string, string) (string, bool) { return "web result", true }
			defer func() { webtools.ExecuteHook = nil }()
			events := make(chan UsageEvent, 1)
			f.h.OnUsage = func(ev UsageEvent) { events <- ev }
			w := httptest.NewRecorder()
			payload, err := jsonx.UnmarshalObj([]byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"search"}],"stream":true}`))
			if err != nil {
				t.Fatal(err)
			}
			q := &request{r: httptest.NewRequest(http.MethodPost, "/", nil), w: w, payload: payload,
				model: "glm-5.3", realm: "intl", start: time.Now()}
			body := strings.NewReader(webToolCallSSE)
			if stream {
				f.h.responsesStream(q, f.pool.All()[0], func() {}, time.Second, body, q.model, responses.Info{WebTools: true})
			} else {
				f.h.responsesWhole(q, f.pool.All()[0], func() {}, time.Second, body, q.model, responses.Info{WebTools: true})
			}
			text := w.Body.String()
			if strings.Contains(text, "secret upstream token") || strings.Contains(text, "response.completed") || strings.Contains(text, "data: [DONE]") {
				t.Errorf("unsafe follow-up failure: %s", text)
			}
			wantOutcome := "failed"
			if stream {
				wantOutcome = "upstream_aborted"
				if w.Code != http.StatusOK || !strings.Contains(text, "event: error") {
					t.Errorf("missing stream error: status=%d body=%s", w.Code, text)
				}
			} else if w.Code != http.StatusBadGateway || !strings.Contains(text, "web tool follow-up failed") {
				t.Errorf("wrong follow-up error: status=%d body=%s", w.Code, text)
			}
			if ev := <-events; ev.Outcome != wantOutcome || ev.Status != http.StatusBadGateway {
				t.Errorf("usage = %+v", ev)
			}
		})
	}
}
