package openai

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReadUntilSemanticReplaysPrelude(t *testing.T) {
	q := &request{}
	body := strings.NewReader(
		"event: message_start\n" +
			`data: {"choices":[{"delta":{"role":"assistant"}}]}` + "\n\n" +
			`data: {"choices":[{"delta":{"content":"hello"}}]}` + "\n\n")
	r, err := readUntilSemantic(q, body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"content":"hello"`) {
		t.Fatalf("prelude was not replayed: %q", got)
	}
}

func TestReadUntilSemanticRejectsEmptyStream(t *testing.T) {
	q := &request{}
	_, err := readUntilSemantic(q, strings.NewReader(`data: {"choices":[{"delta":{"role":"assistant"}}]}`+"\n"))
	if !errors.Is(err, errFirstOutputClosed) {
		t.Fatalf("want closed-before-output, got %v", err)
	}
}

func TestReadUntilSemanticResponsesAndAnthropic(t *testing.T) {
	q := &request{}
	ok, err := classifyFirstOutputLine(q, []byte(`data: {"type":"response.output_text.delta","delta":"hi"}`))
	if err != nil || !ok {
		t.Fatalf("responses delta: ok=%v err=%v", ok, err)
	}
	a := &request{anth: true}
	ok, err = classifyFirstOutputLine(a, []byte(`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`))
	if err != nil || !ok {
		t.Fatalf("anthropic delta: ok=%v err=%v", ok, err)
	}
}

func TestStageFirstOutputTimesOut(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	q := &request{r: httptest.NewRequest("POST", "/v1/chat/completions", nil)}
	h := &Handler{FirstOutputTimeout: 40 * time.Millisecond}
	start := time.Now()
	_, err := h.stageFirstOutput(q, pr, func() {})
	if !errors.Is(err, errFirstOutputTimeout) {
		t.Fatalf("want first-output timeout, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("timeout took %v", elapsed)
	}
}

func TestStageFirstOutputDisabled(t *testing.T) {
	q := &request{r: httptest.NewRequest("POST", "/v1/chat/completions", nil)}
	h := &Handler{}
	body := strings.NewReader("anything")
	got, err := h.stageFirstOutput(q, body, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if got != body {
		t.Fatal("disabled staging must return the original reader")
	}
}

func TestStageFirstOutputHonorsRequestCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	q := &request{r: httptest.NewRequest("POST", "/v1/chat/completions", nil).WithContext(ctx)}
	h := &Handler{FirstOutputTimeout: 5 * time.Second}
	pr, pw := io.Pipe()
	defer pw.Close()
	cancel()
	_, err := h.stageFirstOutput(q, pr, func() {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context cancellation, got %v", err)
	}
}
