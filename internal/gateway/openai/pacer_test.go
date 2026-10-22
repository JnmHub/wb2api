package openai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wb2api/internal/store/settings"
)

func TestStreamTPSResolution(t *testing.T) {
	st := settings.Open(t.TempDir())
	if err := st.SetOutputTPSLimit(25); err != nil {
		t.Fatal(err)
	}
	h := &Handler{Settings: st}
	inherit := &settings.APIKey{TPSLimit: 0}
	if got := h.streamTPS(inherit); got != 25 {
		t.Fatalf("0 must follow global: %v", got)
	}
	off := &settings.APIKey{TPSLimit: -1}
	if got := h.streamTPS(off); got != 0 {
		t.Fatalf("-1 must disable: %v", got)
	}
	over := &settings.APIKey{TPSLimit: 7}
	if got := h.streamTPS(over); got != 7 {
		t.Fatalf("per-key override: %v", got)
	}
}

func TestStreamPacerAverageRate(t *testing.T) {
	p := newStreamPacer(100)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	p.wait(ctx, 1) // first chunk: immediate
	start := time.Now()
	if err := p.wait(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Fatalf("10 tokens at 100 t/s returned after %v", elapsed)
	}
}

func TestStreamPacerOffAndCancel(t *testing.T) {
	if p := newStreamPacer(0); p != nil {
		t.Fatal("0 disables pacing")
	}
	p := newStreamPacer(1)
	ctx, cancel := context.WithCancel(context.Background())
	p.wait(ctx, 1)
	cancel()
	start := time.Now()
	if err := p.wait(ctx, 10); err == nil {
		t.Fatal("a cancelled request must stop waiting")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("cancellation must interrupt the wait")
	}
}

func TestStreamPacerFlushesOnlyBeforeSleep(t *testing.T) {
	ctx := context.Background()
	p := newStreamPacer(100)
	calls := 0
	before := func() error { calls++; return nil }
	if err := p.waitBefore(ctx, 1, before); err != nil || calls != 0 {
		t.Fatalf("first chunk: callbacks=%d err=%v", calls, err)
	}
	p.start = time.Now().Add(-time.Second)
	if err := p.waitBefore(ctx, 1, before); err != nil || calls != 0 {
		t.Fatalf("catch-up chunk: callbacks=%d err=%v", calls, err)
	}
	p.start = time.Now()
	want := errors.New("flush failed")
	if err := p.waitBefore(ctx, 1, func() error { calls++; return want }); !errors.Is(err, want) || calls != 1 {
		t.Fatalf("sleep flush: callbacks=%d err=%v", calls, err)
	}
	// Flushing can itself satisfy the delay; do not sleep for that time again.
	if err := p.waitBefore(ctx, 1, func() error { p.start = time.Now().Add(-time.Second); return nil }); err != nil {
		t.Fatal(err)
	}
}

// TestOutputTPSHighConcurrency drives many independent paced streams through
// the real gateway path: every response must stay complete while the observed
// duration reflects the configured token rate.
func TestOutputTPSHighConcurrency(t *testing.T) {
	const chunks = 40
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < chunks; i++ {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		}
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":40,\"total_tokens\":41}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	e := newEnv(t, up.Config.Handler, 1, `{"output_tps_limit":100}`)
	const streams = 50
	var wg sync.WaitGroup
	errs := make(chan string, streams)
	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
			start := time.Now()
			resp, err := http.Post(e.srv.URL, "application/json", strings.NewReader(body))
			if err != nil {
				errs <- err.Error()
				return
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				errs <- fmt.Sprintf("status %d", resp.StatusCode)
				return
			}
			if got := strings.Count(string(raw), `"content":"x"`); got != chunks {
				errs <- fmt.Sprintf("got %d chunks, want %d", got, chunks)
				return
			}
			if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
				errs <- fmt.Sprintf("stream completed in %v despite 40 tokens at 100 t/s", elapsed)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
