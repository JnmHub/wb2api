package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
)

func TestTrimmedChatReportsOriginalUsageAndPreservesDetails(t *testing.T) {
	const usage = `{"prompt_tokens":12,"completion_tokens":5,"total_tokens":17,` +
		`"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":2},` +
		`"prompt_cache_hit_tokens":4,"vendor_metrics":{"credits":3}}`
	var wantUsage map[string]any
	if err := json.Unmarshal([]byte(usage), &wantUsage); err != nil {
		t.Fatal(err)
	}
	const limit = 1 << 20
	old := strings.Repeat("x", limit)
	for _, provider := range []struct{ name, model string }{
		{realm.CodeArts, "GLM-5.2"},
		{realm.Raccoon, "sn-glm-5-3"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", provider.name, stream), func(t *testing.T) {
				up := func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, `data: {"id":"usage-test","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`+"\n\n")
					fmt.Fprint(w, `data: {"id":"usage-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
					fmt.Fprintf(w, `data: {"id":"usage-test","choices":[],"usage":%s}`+"\n\ndata: [DONE]\n\n", usage)
				}
				var h *Handler
				var gatewayURL string
				var outbound func() []byte
				var events <-chan UsageEvent
				if provider.name == realm.CodeArts {
					received := make(chan struct{}, 1)
					e := newCodeartsEnv(t, func(w http.ResponseWriter, r *http.Request) {
						select {
						case received <- struct{}{}:
						default:
							t.Error("unexpected extra CodeArts request")
						}
						up(w, r)
					})
					h, gatewayURL = e.h, e.srv.URL
					events = e.events
					outbound = func() []byte {
						<-received
						return e.body
					}
				} else {
					received := make(chan []byte, 1)
					e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Errorf("read outbound request: %v", err)
						}
						select {
						case received <- body:
						default:
							t.Error("unexpected extra Raccoon request")
						}
						up(w, r)
					}), 0, "")
					e.h.Pool = pool.New([]*accounts.Record{raccoonAccount("usage-test", provider.model)})
					h, gatewayURL = e.h, e.srv.URL
					events = e.events
					outbound = func() []byte { return <-received }
				}
				if _, err := h.Settings.SetUpstream(map[string]any{provider.name + "_history_limit_mib": 1}); err != nil {
					t.Fatal(err)
				}
				body := fmt.Sprintf(`{"model":%q,"stream":%t,"stream_options":{"include_usage":true},"messages":[`+
					`{"role":"user","content":%q},{"role":"assistant","content":"old answer"},`+
					`{"role":"user","content":"current"}]}`, provider.model, stream, old)
				resp := postJSON(t, gatewayURL+"/v1/chat/completions", body, map[string]string{"X-Realm": provider.name})
				defer resp.Body.Close()
				data, err := io.ReadAll(resp.Body)
				if err != nil || resp.StatusCode != http.StatusOK {
					t.Fatalf("HTTP %d: %s (%v)", resp.StatusCode, data, err)
				}
				wire := outbound()
				var request struct {
					Messages      []struct{ Role, Content string }
					StreamOptions struct {
						IncludeUsage bool `json:"include_usage"`
					} `json:"stream_options"`
				}
				if err := json.Unmarshal(wire, &request); err != nil {
					t.Fatal(err)
				}
				if len(wire) > limit || len(request.Messages) != 1 || request.Messages[0].Content != "current" || !request.StreamOptions.IncludeUsage {
					t.Fatalf("outbound request did not trim history or preserve include_usage: %d bytes, %d messages, include_usage=%t",
						len(wire), len(request.Messages), request.StreamOptions.IncludeUsage)
				}

				frames := [][]byte{data}
				if stream {
					frames = chatSSEPayloads(t, data)
					if len(frames) < 2 || !bytes.Equal(frames[len(frames)-1], []byte("[DONE]")) {
						t.Fatalf("incomplete stream: %s", data)
					}
					frames = frames[:len(frames)-1]
				}
				var seen int
				for _, frame := range frames {
					var payload struct{ Usage map[string]any }
					if err := json.Unmarshal(frame, &payload); err != nil {
						t.Fatal(err)
					}
					if payload.Usage == nil {
						continue
					}
					seen++
					prompt, promptOK := payload.Usage["prompt_tokens"].(float64)
					total, totalOK := payload.Usage["total_tokens"].(float64)
					if !promptOK || !totalOK || prompt <= 12 || total != prompt+5 || payload.Usage["input_tokens_estimated"] != true {
						t.Fatalf("usage did not include an estimated original input count: %#v", payload.Usage)
					}
					payload.Usage["prompt_tokens"] = wantUsage["prompt_tokens"]
					payload.Usage["total_tokens"] = wantUsage["total_tokens"]
					delete(payload.Usage, "input_tokens_estimated")
					if !reflect.DeepEqual(payload.Usage, wantUsage) {
						t.Fatalf("usage details = %#v, want %#v", payload.Usage, wantUsage)
					}
				}
				if seen != 1 {
					t.Fatalf("found %d non-null usage objects, want one: %s", seen, data)
				}
				select {
				case event := <-events:
					var recordedUsage map[string]any
					if err := json.Unmarshal(event.Usage, &recordedUsage); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(recordedUsage, wantUsage) {
						t.Fatalf("recorded usage = %#v, want actual upstream usage %#v", recordedUsage, wantUsage)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("missing usage event")
				}
			})
		}
	}
}
