package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/gateway/responses"
	"wb2api/internal/jsonx"
)

func latencyBegin(ctx context.Context) {
	beginAttempt(ctx)
	l := latencyFrom(ctx)
	l.mu.Lock()
	// Windows clocks may coalesce consecutive time.Now calls. Keep tests
	// deterministic without inventing positive durations in production.
	l.dispatched = l.dispatched.Add(-time.Millisecond)
	l.mu.Unlock()
}

func latencyRequest(t *testing.T) *http.Request {
	t.Helper()
	r, stop := startLatency(httptest.NewRequest(http.MethodPost, "/", nil), time.Now().Add(-time.Second))
	t.Cleanup(stop)
	return r
}

const latencySemantic = `data: {"choices":[{"index":0,"delta":{"content":"hello"}}]}` + "\n\n"
const latencyRole = `data: {"choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n"
const latencyEnd = "data: [DONE]\n\n"

func TestLatencyAttemptResetAndStaleObserver(t *testing.T) {
	r := latencyRequest(t)
	ctx := r.Context()
	addAdmissionWait(ctx, 11*time.Millisecond)
	addKeyWait(ctx, 7*time.Millisecond)
	addKeyWait(ctx, -time.Second)
	latencyBegin(ctx)
	old := upstreamObserver(ctx)
	old([]byte(latencySemantic))
	var first UsageEvent
	applyLatency(ctx, &first)
	if first.UpstreamTTFT <= 0 {
		t.Fatalf("first attempt not measured: %+v", first)
	}
	latencyBegin(ctx)
	old([]byte(latencySemantic))
	var reset UsageEvent
	applyLatency(ctx, &reset)
	if reset.UpstreamTTFT != 0 {
		t.Fatalf("stale reader contaminated retry: %+v", reset)
	}
	fresh := upstreamObserver(ctx)
	fresh([]byte(latencyRole))
	applyLatency(ctx, &reset)
	if reset.UpstreamTTFT != 0 {
		t.Fatalf("role counted as semantic: %+v", reset)
	}
	fresh([]byte(latencySemantic))
	l := latencyFrom(ctx)
	l.mu.Lock()
	semantic := l.semantic
	l.mu.Unlock()
	fresh([]byte(latencySemantic))
	l.mu.Lock()
	if !l.semantic.Equal(semantic) {
		t.Error("duplicate semantic read changed timestamp")
	}
	l.mu.Unlock()
	applyLatency(ctx, &reset)
	if reset.UpstreamTTFT <= 0 || reset.QueueWait != 18*time.Millisecond || reset.AdmissionWait != 11*time.Millisecond || reset.KeyWait != 7*time.Millisecond {
		t.Fatalf("wrong additive attempt metrics: %+v", reset)
	}
	if reset.ClientTTFT != 0 || reset.CancelLatency != 0 {
		t.Fatalf("invented unavailable metrics: %+v", reset)
	}
}

func TestLatencyNoSemanticNoUpstreamSample(t *testing.T) {
	r := latencyRequest(t)
	latencyBegin(r.Context())
	note := upstreamObserver(r.Context())
	for _, line := range []string{latencyRole, ": heartbeat\n", `data: {"usage":{"total_tokens":1},"choices":[]}`, `data: {"choices":[{"delta":{"content":""},"finish_reason":"stop"}]}`, latencyEnd, `data: {"error":"failed"}`} {
		note([]byte(line))
	}
	var ev UsageEvent
	applyLatency(r.Context(), &ev)
	if ev.UpstreamTTFT != 0 || ev.ClientTTFT != 0 {
		t.Fatalf("nonsemantic sample: %+v", ev)
	}
	// A semantic read without a dispatch hook cannot manufacture upstream TTFT.
	noDispatch := latencyRequest(t)
	upstreamObserver(noDispatch.Context())([]byte(latencySemantic))
	applyLatency(noDispatch.Context(), &ev)
	if ev.UpstreamTTFT != 0 {
		t.Fatalf("missing dispatch fabricated metric: %+v", ev)
	}
}

func TestLatencyStageRecordsBeforeReplay(t *testing.T) {
	r := latencyRequest(t)
	latencyBegin(r.Context())
	q := &request{r: r}
	h := &Handler{FirstOutputTimeout: time.Second}
	body, err := h.stageFirstOutput(q, strings.NewReader(latencyRole+latencySemantic+latencyEnd), func() {})
	if err != nil {
		t.Fatal(err)
	}
	l := latencyFrom(r.Context())
	l.mu.Lock()
	semantic := l.semantic
	l.mu.Unlock()
	if semantic.IsZero() {
		t.Fatal("staging returned before timestamping semantic read")
	}
	if _, err := io.ReadAll(body); err != nil {
		t.Fatal(err)
	}
	upstreamObserver(r.Context())([]byte(latencySemantic))
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.semantic.Equal(semantic) {
		t.Fatal("prelude replay changed upstream timestamp")
	}
}

func TestLatencyFlushCancellationAndLegacyFields(t *testing.T) {
	r := latencyRequest(t)
	ctx := r.Context()
	l := latencyFrom(ctx)
	now := time.Now()
	l.mu.Lock()
	l.dispatched = now.Add(-30 * time.Millisecond)
	l.semantic = now.Add(-20 * time.Millisecond)
	l.cancelled = now.Add(-5 * time.Millisecond)
	l.mu.Unlock()
	noteClientFlush(ctx)
	noteClientFlush(ctx)
	ev := UsageEvent{TTFT: 9 * time.Second, Gen: time.Second}
	applyLatency(ctx, &ev)
	if ev.TTFT != 9*time.Second || ev.Gen != time.Second {
		t.Fatalf("legacy metrics changed: %+v", ev)
	}
	if ev.UpstreamTTFT != 10*time.Millisecond || ev.ClientTTFT < time.Second || ev.CancelLatency < 5*time.Millisecond {
		t.Fatalf("bad measurements: %+v", ev)
	}
	if positiveElapsed(now, now.Add(-time.Second)) != 0 || positiveElapsed(time.Time{}, now) != 0 {
		t.Fatal("negative/unknown duration escaped")
	}
}

func TestLatencyEmissionOnce(t *testing.T) {
	r := latencyRequest(t)
	var n atomic.Int32
	h := &Handler{OnUsage: func(UsageEvent) { n.Add(1) }}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); h.emitWithLatency(r.Context(), UsageEvent{}) }()
	}
	wg.Wait()
	if n.Load() != 1 {
		t.Fatalf("emitted %d events", n.Load())
	}
}

func TestLatencyCancelledContextObserver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r, stop := startLatency(httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx), time.Now())
	defer stop()
	cancel()
	// Synchronize the observer without sleeps or assuming callback scheduling.
	l := latencyFrom(r.Context())
	deadline := time.After(time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		l.mu.Lock()
		observed := !l.cancelled.IsZero()
		l.mu.Unlock()
		if observed {
			break
		}
		select {
		case <-deadline:
			t.Fatal("cancellation was not observed")
		case <-tick.C:
		}
	}
	var ev UsageEvent
	applyLatency(r.Context(), &ev)
	if ev.CancelLatency <= 0 {
		t.Fatalf("missing observed cancellation: %+v", ev)
	}
}

func TestLatencyThreeProtocolStreamsAndWhole(t *testing.T) {
	for _, protocol := range []string{"chat", "messages", "responses"} {
		for _, stream := range []bool{false, true} {
			name := protocol + "/whole"
			if stream {
				name = protocol + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				r := latencyRequest(t)
				start := latencyFrom(r.Context()).start
				latencyBegin(r.Context())
				var events []UsageEvent
				h := &Handler{OnUsage: func(ev UsageEvent) { events = append(events, ev) }}
				w := httptest.NewRecorder()
				q := &request{r: r, w: w, payload: jsonx.NewObj(), start: start}
				body := strings.NewReader(latencyRole + latencySemantic + latencyEnd)
				switch protocol {
				case "chat":
					if stream {
						h.pipeStream(w, r, func() {}, time.Minute, body, testAccount(), "m", "", "", start, 0)
					} else {
						h.aggregate(w, r, func() {}, time.Minute, body, testAccount(), "m", "", "", start)
					}
				case "messages":
					if stream {
						h.messagesStream(q, testAccount(), func() {}, time.Minute, body, "m")
					} else {
						h.messagesWhole(q, testAccount(), func() {}, time.Minute, body, "m")
					}
				case "responses":
					if stream {
						h.responsesStream(q, testAccount(), func() {}, time.Minute, body, "m", responses.Info{})
					} else {
						h.responsesWhole(q, testAccount(), func() {}, time.Minute, body, "m", responses.Info{})
					}
				}
				if len(events) != 1 {
					t.Fatalf("events=%d", len(events))
				}
				ev := events[0]
				if ev.UpstreamTTFT <= 0 || ev.UpstreamTTFT >= ev.TTFT || ev.TTFT < time.Second {
					t.Fatalf("attempt/request metrics not separated: %+v", ev)
				}
				// Streaming records the first semantic frame; nonstream now
				// explicitly flushes the complete JSON document under a write
				// deadline. Both are real Flush calls, not inferred socket receipt.
				if !w.Flushed || ev.ClientTTFT < time.Second {
					t.Fatalf("missing successful output Flush: %+v flushed=%t", ev, w.Flushed)
				}
				if ev.CancelLatency != 0 {
					t.Fatalf("successful delivery invented cancellation: %+v", ev)
				}
			})
		}
	}
}
