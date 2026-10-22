package openai

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wb2api/internal/gateway/responses"
	"wb2api/internal/jsonx"
)

type latencyFlushFailureWriter struct{ *httptest.ResponseRecorder }

func (*latencyFlushFailureWriter) FlushError() error { return errors.New("downstream flush failed") }

func TestLatencyFailedSemanticFlushNotClientTTFT(t *testing.T) {
	for _, protocol := range []string{"messages", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			r := latencyRequest(t)
			latencyBegin(r.Context())
			var events []UsageEvent
			h := &Handler{OnUsage: func(ev UsageEvent) { events = append(events, ev) }}
			w := &latencyFlushFailureWriter{httptest.NewRecorder()}
			q := &request{r: r, w: w, payload: jsonx.NewObj(), start: latencyFrom(r.Context()).start}
			body := strings.NewReader(latencySemantic + latencyEnd)
			if protocol == "messages" {
				h.messagesStream(q, testAccount(), func() {}, time.Minute, body, "m")
			} else {
				h.responsesStream(q, testAccount(), func() {}, time.Minute, body, "m", responses.Info{})
			}
			if len(events) != 1 {
				t.Fatalf("events=%d", len(events))
			}
			if ev := events[0]; ev.ClientTTFT != 0 || ev.Outcome != "client_aborted" || ev.Status != http.StatusOK {
				t.Fatalf("failed Flush reported success: %+v", ev)
			}
		})
	}
}
