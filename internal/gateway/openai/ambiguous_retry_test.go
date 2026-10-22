package openai

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the public adapters, selection/rotation loop, wire transport and
// terminal error accounting together. Closing only after ReadAll proves that
// this POST may already have been processed, not merely that a dial failed.
func TestAmbiguousRetryStopsAllProtocols(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	const model = "deepseek-v4.1-flash"
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, prior429 := range []bool{false, true} {
				name := fmt.Sprintf("%s/stream=%t/prior429=%t", protocol, stream, prior429)
				t.Run(name, func(t *testing.T) {
					var calls, ambiguousCalls, paidCalls atomic.Int32
					var e *env
					up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						defer r.Body.Close()
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Errorf("upstream did not consume POST: %v", err)
							return
						}
						if r.Method != http.MethodPost || len(body) == 0 {
							t.Errorf("expected complete nonempty POST, got %s %q", r.Method, body)
						}
						n := calls.Add(1)
						if strings.Contains(string(body), model+"-sg") {
							paidCalls.Add(1)
						}
						if prior429 && n == 1 {
							w.WriteHeader(http.StatusTooManyRequests)
							fmt.Fprint(w, `{"code":6004,"msg":"usage exceeds frequency limit"}`)
							return
						}
						ambiguousCalls.Add(1)
						// Make paid fallback genuinely eligible at the exact failure
						// boundary (e.g. another worker just learned these caps). The
						// abort guard must suppress it, not allCapped returning false.
						now := time.Now()
						for _, acc := range e.h.Pool.All() {
							acc.NoteModelCap(model, now.Add(time.Hour), now)
						}
						c, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Errorf("hijack upstream: %v", err)
							return
						}
						c.Close() // entire POST consumed, no response headers sent
					})
					e = newEnv(t, up, 3, `{"free_model_credit_fallback":true,"auto_switch_product":false}`)
					e.h.MaxAttempts = 9
					var handler http.Handler = e.h
					body := fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":"private-prompt"}]}`, model, stream)
					if protocol == "responses" {
						handler = &Responses{Chat: e.h}
						body = fmt.Sprintf(`{"model":%q,"stream":%t,"input":"private-prompt"}`, model, stream)
					} else if protocol == "messages" {
						handler = &Messages{Chat: e.h}
						body = fmt.Sprintf(`{"model":%q,"stream":%t,"max_tokens":10,"messages":[{"role":"user","content":"private-prompt"}]}`, model, stream)
					}
					srv := httptest.NewServer(handler)
					defer srv.Close()
					client := &http.Client{Timeout: 5 * time.Second}
					resp, err := client.Post(srv.URL, "application/json", strings.NewReader(body))
					if err != nil {
						t.Fatal(err)
					}
					defer resp.Body.Close()
					got, err := io.ReadAll(resp.Body)
					if err != nil {
						t.Fatal(err)
					}
					if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(got), "outcome unknown") || !strings.Contains(string(got), "did not replay") {
						t.Fatalf("ambiguous failure must override even earlier 429: status=%d body=%s", resp.StatusCode, got)
					}
					if resp.Header.Get("Retry-After") != "" {
						t.Fatal("ambiguous outcome incorrectly advertised as retryable rate limit")
					}
					assertNoPrivateError(t, string(got))
					if strings.Contains(string(got), "127.0.0.1") || strings.Contains(string(got), "unexpected EOF") || strings.Contains(string(got), "wsarecv") {
						t.Fatalf("transport detail exposed: %s", got)
					}
					if protocol == "messages" && !strings.Contains(string(got), `"type":"error"`) {
						t.Fatalf("Anthropic error envelope lost: %s", got)
					}
					want := int32(1)
					if prior429 {
						want++
					}
					if calls.Load() != want || ambiguousCalls.Load() != 1 || paidCalls.Load() != 0 {
						t.Fatalf("unsafe rotation/fallback: posts=%d ambiguous=%d paid=%d", calls.Load(), ambiguousCalls.Load(), paidCalls.Load())
					}
					if !e.h.allCapped("intl", model, time.Now()) {
						t.Fatal("test did not establish eligible paid fallback")
					}
					ev := e.event(t)
					if ev.Outcome != "failed" || ev.Status != http.StatusBadGateway || ev.Account == "" || ev.Model != model || ev.Stream != stream {
						t.Fatalf("wrong terminal usage emission: %+v", ev)
					}
					select {
					case extra := <-e.events:
						t.Fatalf("duplicate usage emission: %+v", extra)
					default:
					}
				})
			}
		}
	}
}
