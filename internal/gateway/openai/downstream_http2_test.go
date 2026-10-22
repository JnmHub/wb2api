package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/store/settings"
)

type downstreamH2ConnKey struct{}

// Unwrap preserves ResponseController's real HTTP/2 per-stream deadlines. The
// notification witnesses output beyond the client's stream receive window; it
// does not replace the actual network write or implement its own timeout.
type downstreamH2Observer struct {
	http.ResponseWriter
	bytes   int
	pending chan struct{}
	once    sync.Once
}

func (w *downstreamH2Observer) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *downstreamH2Observer) Write(b []byte) (int, error) {
	if w.bytes+len(b) > 4<<20 {
		w.once.Do(func() { close(w.pending) })
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// net/http's HTTP/2 transport advertises 4MiB of stream receive credit and 1GiB
// of connection credit (transportDefaultStreamFlow/transportDefaultConnFlow).
// Eight MiB exceeds the unread stream's credit without exhausting connection
// credit, so a sibling stream on the SAME TCP connection must remain usable.
func TestDownstreamHTTP2SlowStreamIsolation(t *testing.T) {
	for _, stream := range []bool{true, false} {
		for _, cancelBlocked := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/cancel=%t", stream, cancelBlocked), func(t *testing.T) {
				fastEntered, fastRelease := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				unblockFast := func() { releaseOnce.Do(func() { close(fastRelease) }) }
				defer unblockFast()
				var upstreamCalls atomic.Int32
				e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					upstreamCalls.Add(1)
					body, err := io.ReadAll(r.Body)
					if err != nil {
						return
					}
					if strings.Contains(string(body), "fast-sibling") {
						close(fastEntered)
						select {
						case <-fastRelease:
							sseUpstream("sibling survived")(w, r)
						case <-r.Context().Done():
						}
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					// Report a known usage record before content so streaming and
					// whole-response abort accounting can both preserve paid usage.
					_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
					frame := relaySSE(strings.Repeat("x", 64<<10))
					for i := 0; i < 128; i++ {
						if _, err := io.WriteString(w, frame); err != nil {
							return
						}
						if err := http.NewResponseController(w).Flush(); err != nil {
							return
						}
					}
					_, _ = io.WriteString(w, "data: [DONE]\n\n")
				}), 2, "")
				t.Cleanup(unblockFast) // precedes fixture server shutdown
				e.h.AccountInflight = 1
				e.h.DownstreamWriteTimeout = 750 * time.Millisecond
				if cancelBlocked {
					e.h.DownstreamWriteTimeout = time.Hour
				}
				slots := &downstreamSlotProbe{}
				e.h.Limiter = slots
				cap := 1
				slowName, fastName := "h2-slow", "h2-fast"
				slowKey, err := e.h.Settings.CreateKey(settings.KeyInput{Name: &slowName, ConcurrencyLimit: &cap})
				if err != nil {
					t.Fatal(err)
				}
				fastKey, err := e.h.Settings.CreateKey(settings.KeyInput{Name: &fastName, ConcurrencyLimit: &cap})
				if err != nil {
					t.Fatal(err)
				}
				pending, slowDone, fastDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
				cancelSlow := make(chan context.CancelFunc, 1)
				slowConn, fastConn := make(chan int64, 1), make(chan int64, 1)
				var connections atomic.Int64
				srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.ProtoMajor != 2 {
						t.Errorf("server protocol=%s; HTTP/2 required", r.Proto)
					}
					id := r.Context().Value(downstreamH2ConnKey{}).(int64)
					if r.Header.Get("X-Test-Stream") == "slow" {
						slowConn <- id
						ctx, cancel := context.WithCancel(r.Context())
						defer cancel()
						cancelSlow <- cancel
						e.h.ServeHTTP(&downstreamH2Observer{ResponseWriter: w, pending: pending}, r.WithContext(ctx))
						close(slowDone)
					} else {
						fastConn <- id
						e.h.ServeHTTP(w, r)
						close(fastDone)
					}
				}))
				srv.EnableHTTP2 = true
				srv.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
					return context.WithValue(ctx, downstreamH2ConnKey{}, connections.Add(1))
				}
				srv.StartTLS()
				defer func() { unblockFast(); srv.Close() }()
				transport := srv.Client().Transport.(*http.Transport).Clone()
				transport.ForceAttemptHTTP2 = true
				transport.MaxConnsPerHost = 1
				defer transport.CloseIdleConnections()
				client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
				post := func(label, key string, wantStream bool) (*http.Response, error) {
					b := fmt.Sprintf(`{"model":"glm-5.3","stream":%t,"messages":[{"role":"user","content":%q}]}`, wantStream, label+"-sibling")
					r, err := http.NewRequest("POST", srv.URL, strings.NewReader(b))
					if err != nil {
						return nil, err
					}
					r.Header.Set("Content-Type", "application/json")
					r.Header.Set("Authorization", "Bearer "+key)
					r.Header.Set("X-Test-Stream", label)
					return client.Do(r)
				}
				slow, err := post("slow", slowKey.Key, stream)
				if err != nil {
					t.Fatal(err)
				}
				defer slow.Body.Close()
				if slow.ProtoMajor != 2 || slow.StatusCode != 200 {
					t.Fatalf("slow response: %s status=%d", slow.Proto, slow.StatusCode)
				}
				// Deliberately never read slow.Body. Transport buffers alone cannot
				// absorb eight MiB; flow-control blocks the actual server writer.
				type result struct {
					body          string
					proto, status int
					err           error
				}
				fastResult := make(chan result, 1)
				go func() {
					resp, err := post("fast", fastKey.Key, false)
					if err != nil {
						fastResult <- result{err: err}
						return
					}
					defer resp.Body.Close()
					b, err := io.ReadAll(resp.Body)
					fastResult <- result{body: string(b), proto: resp.ProtoMajor, status: resp.StatusCode, err: err}
				}()
				select {
				case <-fastEntered:
				case <-time.After(5 * time.Second):
					t.Fatal("sibling did not enter upstream while slow response unread")
				}
				select {
				case <-pending:
				case <-time.After(5 * time.Second):
					t.Fatal("slow relay did not attempt output beyond HTTP/2 stream window")
				}
				if cancelBlocked {
					select {
					case cancel := <-cancelSlow:
						cancel()
					case <-time.After(time.Second):
						t.Fatal("missing slow cancellation handle")
					}
				}
				select {
				case <-slowDone:
				case <-time.After(5 * time.Second):
					t.Fatal("HTTP/2 slow write did not unwind")
				}
				// Keep the sibling active across the slow stream's timeout/reset.
				select {
				case <-fastDone:
					t.Fatal("held sibling was canceled by another stream's deadline")
				default:
				}
				if slots.acquired.Load() != 2 || slots.released.Load() != 1 {
					t.Fatalf("while sibling held: acquired=%d released=%d", slots.acquired.Load(), slots.released.Load())
				}
				e.h.keyMu.Lock()
				sg, fg := e.h.keyGates[slowKey.ID], e.h.keyGates[fastKey.ID]
				gatesOK := sg != nil && fg != nil && sg.active == 0 && fg.active == 1
				e.h.keyMu.Unlock()
				if !gatesOK {
					t.Fatal("slow key retained or sibling key incorrectly released")
				}
				unblockFast()
				select {
				case got := <-fastResult:
					if got.err != nil || got.proto != 2 || got.status != 200 || !strings.Contains(got.body, "sibling survived") {
						t.Fatalf("sibling failed after slow stream aborted: %+v", got)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("sibling did not complete after release")
				}
				select {
				case <-fastDone:
				case <-time.After(time.Second):
					t.Fatal("sibling handler not finalized")
				}
				if a, b := <-slowConn, <-fastConn; a != b || connections.Load() != 1 {
					t.Fatalf("test did not use same HTTP/2 TCP connection: slow=%d fast=%d connections=%d", a, b, connections.Load())
				}
				if upstreamCalls.Load() != 2 || slots.released.Load() != 2 {
					t.Fatalf("unexpected replay/leak: calls=%d releases=%d", upstreamCalls.Load(), slots.released.Load())
				}
				for _, acc := range e.h.Pool.All() {
					release, ok := acc.TryAcquire(1)
					if !ok {
						t.Fatal("retained local account slot")
					}
					release()
				}
				seenSlow, seenFast := false, false
				for i := 0; i < 2; i++ {
					ev := e.event(t)
					if ev.KeyID == slowKey.ID {
						seenSlow = true
						if ev.Outcome != "client_aborted" || ev.Stream != stream {
							t.Fatalf("slow outcome=%+v", ev)
						}
						var u struct {
							Total int `json:"total_tokens"`
						}
						if err := json.Unmarshal(ev.Usage, &u); err != nil || u.Total != 15 {
							t.Fatalf("paid usage not retained: %s err=%v", ev.Usage, err)
						}
					} else if ev.KeyID == fastKey.ID {
						seenFast = true
						if ev.Outcome != "completed" {
							t.Fatalf("sibling outcome=%+v", ev)
						}
					} else {
						t.Fatalf("unexpected usage key=%s", ev.KeyID)
					}
				}
				if !seenSlow || !seenFast {
					t.Fatal("missing distinct terminal usage events")
				}
				select {
				case ev := <-e.events:
					t.Fatalf("duplicate usage event: %+v", ev)
				default:
				}
			})
		}
	}
}
