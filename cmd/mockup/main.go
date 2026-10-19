// Command mockup is a stand-in for the WorkBuddy upstream, used for load tests
// and differential tests. It speaks just enough of /v2/chat/completions:
// OpenAI-style SSE with the upstream's habit of padding deltas with empty
// fields, a final usage chunk, and a few failure modes selected by model name.
//
//	model contains "think"  -> prose containing a <thinking> tag at token 20
//	model contains "m429"   -> 429 with a 6004-style body
//	model contains "m503"   -> 503
//	model contains "mhang"  -> accepts the request and never answers
//	model contains "mtool"  -> prints a tool call as text (leaked call)
//
// GET /v3/config answers with a tiny product-config catalogue.
//
// Per-request knobs (headers): X-Mock-Tokens, X-Mock-TPS, X-Mock-Delay (ms
// before response headers).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var (
	active atomic.Int64
	peak   atomic.Int64
	total  atomic.Int64
	chunks atomic.Int64
)

func headerInt(r *http.Request, name string, def int) int {
	if v, err := strconv.Atoi(r.Header.Get(name)); err == nil && v >= 0 {
		return v
	}
	return def
}

func chunkLine(i int, content string) []byte {
	b, _ := json.Marshal(content)
	return []byte(fmt.Sprintf(`data: {"id":"mock-1","object":"chat.completion.chunk","created":1790000000,"model":"mock","choices":[{"index":0,"delta":{"role":"assistant","content":%s,"reasoning_content":"","tool_calls":[],"refusal":null},"finish_reason":""}]}`+"\n\n", b))
}

func handleChat(defTokens, defTPS int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, `{"code":401,"msg":"missing token"}`, http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)
		n := active.Add(1)
		defer active.Add(-1)
		total.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		if d := headerInt(r, "X-Mock-Delay", 0); d > 0 {
			select {
			case <-time.After(time.Duration(d) * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
		switch {
		case strings.Contains(req.Model, "mhang"):
			<-r.Context().Done()
			return
		case strings.Contains(req.Model, "m429"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"code":6004,"msg":"usage exceeds frequency limit, usage will reset at 2099-01-01 00:00:00"}`))
			return
		case strings.Contains(req.Model, "m503"):
			http.Error(w, `{"code":503,"msg":"unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		tokens := headerInt(r, "X-Mock-Tokens", defTokens)
		tps := headerInt(r, "X-Mock-TPS", defTPS)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		var tick *time.Ticker
		if tps > 0 {
			tick = time.NewTicker(time.Second / time.Duration(tps))
			defer tick.Stop()
		}
		for i := 0; i < tokens; i++ {
			text := "tok" + strconv.Itoa(i) + " "
			if strings.Contains(req.Model, "think") && i == 20 {
				text = "<thinking>"
			}
			if strings.Contains(req.Model, "mtool") {
				switch i {
				case 0:
					text = "Running it.\n<run_code>"
				case 1:
					text = `<parameter name="code">print(1)</parameter>`
				case 2:
					text = "</run_code>"
				default:
					break
				}
				if i > 2 {
					tokens = 3
					break
				}
			}
			if _, err := w.Write(chunkLine(i, text)); err != nil {
				return
			}
			chunks.Add(1)
			_ = rc.Flush()
			if tick != nil {
				select {
				case <-tick.C:
				case <-r.Context().Done():
					return
				}
			}
		}
		fin := fmt.Sprintf(`data: {"id":"mock-1","object":"chat.completion.chunk","created":1790000000,"model":"mock","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n"+
			`data: {"id":"mock-1","object":"chat.completion.chunk","created":1790000000,"model":"mock","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":%d,"total_tokens":%d,"credit":0.5}}`+"\n\ndata: [DONE]\n\n", tokens, tokens+100)
		_, _ = w.Write([]byte(fin))
		_ = rc.Flush()
	}
}

func main() {
	addr := flag.String("addr", "127.0.0.1:19000", "listen address")
	tokens := flag.Int("tokens", 200, "default tokens per stream")
	tps := flag.Int("tps", 50, "default tokens per second per stream (0 = as fast as possible)")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/chat/completions", handleChat(*tokens, *tps))
	mux.HandleFunc("/v3/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{"agents":[{"name":"cli","models":["glm-5.3","hy3","mock-new","default-model"]}],`+
			`"models":[{"id":"glm-5.3","name":"GLM 5.3 (mock)","credits":"x1.00","supportsToolCall":true},`+
			`{"id":"mock-new","name":"Mock New","credits":"x2.00","supportsImages":true}]}}`)
	})
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]int64{"active": active.Load(), "peak": peak.Load(), "total": total.Load(), "chunks": chunks.Load()})
	})
	srv := &http.Server{Addr: *addr, Handler: mux}
	go func() {
		log.Printf("mock upstream on http://%s (tokens=%d tps=%d)", *addr, *tokens, *tps)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
