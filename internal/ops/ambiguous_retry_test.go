package ops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/upstream"
	"wb2api/internal/upstream/identity"
)

func TestAmbiguousRetryDoJSONDoesNotReplayPOST(t *testing.T) {
	const payload = `{"operation":"apply-once"}`
	for _, phase := range []string{"headers", "body"} {
		t.Run(phase, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer r.Body.Close()
				body, err := io.ReadAll(r.Body)
				if err != nil || r.Method != http.MethodPost || string(body) != payload {
					t.Errorf("POST was not fully consumed: method=%s body=%q err=%v", r.Method, body, err)
					return
				}
				calls.Add(1)
				c, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("hijack: %v", err)
					return
				}
				defer c.Close()
				if phase == "body" {
					// Valid headers then a deliberately truncated JSON response.
					io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n{\"accepted\":")
				}
			}))
			defer srv.Close()
			e := newEnv(t, "intl", nil)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := e.c.doJSON(ctx, e.acc(), http.MethodPost, srv.URL+"/apply", identity.Headers{
				identity.P("Content-Type", "application/json"),
			}, []byte(payload), time.Second, 3)
			if result != nil || !upstream.IsAmbiguousRequest(err) {
				t.Fatalf("possibly accepted POST must return marker: result=%v err=%v", result, err)
			}
			if calls.Load() != 1 {
				t.Fatalf("ambiguous POST replayed despite attempts=3: calls=%d err=%v", calls.Load(), err)
			}
			if phase == "body" && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("body error lost from error chain: %v", err)
			}
			if phase == "headers" && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("header error lost from error chain: %v", err)
			}
			if retryable(fmt.Errorf("operation: %w", err)) {
				t.Fatal("wrapped ambiguous marker remains retryable")
			}
		})
	}
}
