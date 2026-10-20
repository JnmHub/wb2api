package openai

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestLongContextHighConcurrency sends large prompts and large streamed answers
// through the real gateway path to prove that concurrent long-context traffic is
// not truncated, interleaved or capped by the gateway.
func TestLongContextHighConcurrency(t *testing.T) {
	const (
		chunks    = 800
		chunkText = "0123456789abcdef" // 16 bytes
		prompt    = "context-"         // repeated to ~200 KB
		requests  = 100
	)
	want := strings.Repeat(chunkText, chunks)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			fmt.Fprintf(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", chunkText)
			if fl != nil && i%32 == 0 {
				fl.Flush()
			}
		}
		io.WriteString(w, "data: {\"id\":\"c\",\"choices\":[],\"usage\":{\"prompt_tokens\":200000,\"completion_tokens\":12800,\"total_tokens\":212800}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()
	e := newEnv(t, up.Config.Handler, 1, `{}`)
	// The fixture's usage channel is buffered for a handful of requests; this
	// test emits one event per request, so give it room instead of letting the
	// synchronous OnUsage callback block the handler.
	e.events = make(chan UsageEvent, requests+100)
	longPrompt := strings.Repeat(prompt, 200_000/len(prompt))
	bodyFor := func(stream bool) string {
		msg := map[string]any{"role": "user", "content": longPrompt}
		b, _ := json.Marshal(map[string]any{"model": "m", "stream": stream, "messages": []any{msg}})
		return string(b)
	}
	postJSON := func(body string) (int, string, error) {
		resp, err := http.Post(e.srv.URL, "application/json", strings.NewReader(body))
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw), err
	}

	t.Run("non-stream", func(t *testing.T) {
		var wg sync.WaitGroup
		errs := make(chan string, requests)
		for i := 0; i < requests; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				code, body, err := postJSON(bodyFor(false))
				if err != nil {
					errs <- err.Error()
					return
				}
				if code != http.StatusOK {
					errs <- fmt.Sprintf("status %d: %.200s", code, body)
					return
				}
				var out struct {
					Choices []struct {
						Message struct {
							Content string `json:"content"`
						} `json:"message"`
					} `json:"choices"`
				}
				if err := json.Unmarshal([]byte(body), &out); err != nil || len(out.Choices) == 0 {
					errs <- "bad json: " + err.Error()
					return
				}
				if got := out.Choices[0].Message.Content; got != want {
					errs <- fmt.Sprintf("content %d bytes, want %d", len(got), len(want))
				}
			}()
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Error(e)
		}
	})

	t.Run("stream", func(t *testing.T) {
		const streams = 48
		var wg sync.WaitGroup
		errs := make(chan string, streams)
		for i := 0; i < streams; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req, _ := http.NewRequest(http.MethodPost, e.srv.URL, strings.NewReader(bodyFor(true)))
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					errs <- err.Error()
					return
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					errs <- fmt.Sprintf("status %d", resp.StatusCode)
					return
				}
				var got strings.Builder
				sc := bufio.NewScanner(resp.Body)
				sc.Buffer(make([]byte, 64<<10), 1<<20)
				for sc.Scan() {
					line := sc.Text()
					if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
						continue
					}
					var chunk struct {
						Choices []struct {
							Delta struct {
								Content string `json:"content"`
							} `json:"delta"`
						} `json:"choices"`
					}
					if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk) != nil {
						continue
					}
					for _, c := range chunk.Choices {
						got.WriteString(c.Delta.Content)
					}
				}
				if got.String() != want {
					errs <- fmt.Sprintf("streamed content %d bytes, want %d", got.Len(), len(want))
				}
			}()
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Error(e)
		}
	})
}
