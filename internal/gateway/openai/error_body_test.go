package openai

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type blockingErrorBody struct {
	once sync.Once
	done chan struct{}
}

func (b *blockingErrorBody) Read([]byte) (int, error) {
	<-b.done
	return 0, io.EOF
}

func (b *blockingErrorBody) Close() error {
	b.once.Do(func() { close(b.done) })
	return nil
}

func TestReadUpstreamErrorBodyHasTimeoutAndCancels(t *testing.T) {
	body := &blockingErrorBody{done: make(chan struct{})}
	cancelled := make(chan struct{})
	var cancelOnce sync.Once
	start := time.Now()
	got := readUpstreamErrorBody(body, func() {
		cancelOnce.Do(func() { close(cancelled) })
		_ = body.Close()
	}, 20*time.Millisecond)
	if len(got) != 0 {
		t.Fatalf("body = %q", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("read took too long: %v", elapsed)
	}
	if elapsed := time.Since(start); elapsed < 10*time.Millisecond {
		t.Fatalf("read returned before timeout: %v", elapsed)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("timeout did not cancel upstream read")
	}
}

func TestReadUpstreamErrorBodyCapsBytes(t *testing.T) {
	got := readUpstreamErrorBody(io.NopCloser(strings.NewReader(strings.Repeat("x", maxUpstreamErrorBody+32))), func() {}, time.Second)
	if len(got) != maxUpstreamErrorBody {
		t.Fatalf("body length = %d, want %d", len(got), maxUpstreamErrorBody)
	}
}
