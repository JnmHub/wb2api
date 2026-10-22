package openai

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/gateway/responses"
	"wb2api/internal/jsonx"
	"wb2api/internal/webtools"
)

const responsePartialSSE = "data: {\"id\":\"u\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"part\"}}]}\n\n"

type closeTrackedReader struct {
	io.Reader
	closes atomic.Int32
}

func (r *closeTrackedReader) Close() error {
	r.closes.Add(1)
	return nil
}

func TestResponsesStreamErrorsAreExplicitAndPreservePartialOutput(t *testing.T) {
	for _, kind := range []string{"read", "envelope"} {
		t.Run(kind, func(t *testing.T) {
			var reader io.Reader = &failAfterReader{data: []byte(responsePartialSSE)}
			if kind == "envelope" {
				reader = strings.NewReader(responsePartialSSE + "data: {\"error\":{\"message\":\"secret upstream token\"}}\n\n")
			}
			body := &closeTrackedReader{Reader: reader}
			events := make(chan UsageEvent, 1)
			h := &Handler{OnUsage: func(ev UsageEvent) { events <- ev }}
			w := httptest.NewRecorder()
			q := &request{r: httptest.NewRequest("POST", "/", nil), w: w, payload: jsonx.NewObj(), start: time.Now()}
			h.responsesStream(q, testAccount(), func() {}, time.Second, body, "m", responses.Info{})
			text := w.Body.String()
			if !strings.Contains(text, "response.output_text.delta") || !strings.Contains(text, "part") || !strings.Contains(text, "event: error") {
				t.Fatalf("partial content and error missing: %s", text)
			}
			for _, forbidden := range []string{"response.completed", "data: [DONE]", "secret upstream token", "upstream read failed"} {
				if strings.Contains(text, forbidden) {
					t.Errorf("response contains %q: %s", forbidden, text)
				}
			}
			evs := readEvents(t, w.Result())
			for i, ev := range evs {
				if ev.data["sequence_number"] != float64(i+1) {
					t.Errorf("event %d sequence = %v", i, ev.data["sequence_number"])
				}
			}
			if ev := <-events; ev.Outcome != "upstream_aborted" || ev.Status != http.StatusBadGateway {
				t.Errorf("usage = %+v", ev)
			}
			if n := body.closes.Load(); n != 1 {
				t.Errorf("body closed %d times, want once", n)
			}
		})
	}
}

func TestResponsesFollowUpIdleClosesCurrentRound(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			connected := make(chan struct{}, 1)
			closed := make(chan error, 1)
			up := newRawUpstream(t, func(_ int, _ rawReq, c net.Conn) {
				// Headers are successful, but this follow-up never sends its body.
				fmt.Fprint(c, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 99999\r\n\r\n")
				connected <- struct{}{}
				_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
				var b [1]byte
				_, err := c.Read(b[:])
				closed <- err
			})
			f := newFixture(t, up, accSpec{})
			webtools.ExecuteHook = func(context.Context, string, string) (string, bool) { return "web result", true }
			defer func() { webtools.ExecuteHook = nil }()
			events := make(chan UsageEvent, 1)
			f.h.OnUsage = func(ev UsageEvent) { events <- ev }
			w := httptest.NewRecorder()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			payload, err := jsonx.UnmarshalObj([]byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"search"}],"stream":true}`))
			if err != nil {
				t.Fatal(err)
			}
			q := &request{r: httptest.NewRequest("POST", "/", nil).WithContext(ctx), w: w, payload: payload,
				model: "glm-5.3", realm: "intl", start: time.Now()}
			initial := &closeTrackedReader{Reader: strings.NewReader(webToolCallSSE)}
			firstCtx, cancelFirst := context.WithCancel(ctx)
			defer cancelFirst()
			start := time.Now()
			acc := f.pool.All()[0]
			if stream {
				f.h.responsesStream(q, acc, cancelFirst, 100*time.Millisecond, initial, q.model, responses.Info{WebTools: true})
			} else {
				f.h.responsesWhole(q, acc, cancelFirst, 100*time.Millisecond, initial, q.model, responses.Info{WebTools: true})
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Errorf("follow-up ignored idle budget: %v", elapsed)
			}
			select {
			case <-connected:
			default:
				t.Fatal("follow-up was not opened")
			}
			select {
			case err := <-closed:
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					t.Fatalf("follow-up connection not closed: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("follow-up body was not closed")
			}
			if firstCtx.Err() == nil || initial.closes.Load() != 1 {
				t.Errorf("initial round not released: ctx=%v closes=%d", firstCtx.Err(), initial.closes.Load())
			}
			if ev := <-events; ev.Outcome != "upstream_aborted" {
				t.Errorf("usage outcome = %q", ev.Outcome)
			}
			if stream {
				text := w.Body.String()
				if !strings.Contains(text, "event: error") || strings.Contains(text, "response.completed") || strings.Contains(text, "data: [DONE]") {
					t.Errorf("bad failure stream: %s", text)
				}
			} else if w.Code != http.StatusBadGateway {
				t.Errorf("status = %d, body = %s", w.Code, w.Body.String())
			}
		})
	}
}
