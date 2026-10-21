package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

var (
	errFirstOutputTimeout = errors.New("upstream first output timeout")
	errFirstOutputClosed  = errors.New("upstream stream closed before first output")
	errFirstOutputEvent   = errors.New("upstream error before first output")
)

const maxFirstOutputPrelude = 8 << 20

// stageFirstOutput reads just enough of a streaming response to prove that the
// account can actually produce semantic output. Headers are not sent until this
// succeeds, so a slow or dead account can still be replaced without exposing a
// half-written SSE response. The returned reader replays every byte read here.
//
// A zero timeout keeps the old behavior and does not read ahead.
func (h *Handler) stageFirstOutput(q *request, body io.Reader, cancel context.CancelFunc) (io.Reader, error) {
	if h.FirstOutputTimeout <= 0 {
		return body, nil
	}
	type staged struct {
		r   io.Reader
		err error
	}
	ch := make(chan staged, 1)
	go func() {
		r, err := readUntilSemantic(q, body)
		ch <- staged{r: r, err: err}
	}()

	timer := time.NewTimer(h.FirstOutputTimeout)
	defer timer.Stop()
	select {
	case <-timer.C:
		cancel()
		return nil, errFirstOutputTimeout
	case <-q.r.Context().Done():
		cancel()
		return nil, q.r.Context().Err()
	case out := <-ch:
		return out.r, out.err
	}
}

func readUntilSemantic(q *request, body io.Reader) (io.Reader, error) {
	br := bufio.NewReaderSize(body, 64<<10)
	var prelude bytes.Buffer
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if prelude.Len()+len(line) > maxFirstOutputPrelude {
				return nil, fmt.Errorf("first output prelude exceeds %d bytes", maxFirstOutputPrelude)
			}
			prelude.Write(line)
			ok, serr := classifyFirstOutputLine(q, line)
			if serr != nil {
				return nil, serr
			}
			if ok {
				return io.MultiReader(bytes.NewReader(prelude.Bytes()), br), nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, errFirstOutputClosed
			}
			return nil, err
		}
	}
}

// classifyFirstOutputLine reports whether one SSE line is the first semantic
// output. It recognizes the three stream dialects the gateway serves without
// needing a separate parser for each one.
func classifyFirstOutputLine(q *request, line []byte) (bool, error) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || bytes.HasPrefix(trimmed, []byte("event:")) || bytes.HasPrefix(trimmed, []byte(":")) {
		return false, nil
	}
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) {
		return false, nil
	}
	var obj map[string]any
	if json.Unmarshal(trimmed, &obj) != nil {
		return false, nil
	}
	if e, ok := obj["error"]; ok && e != nil {
		return false, errFirstOutputEvent
	}

	if q.anth {
		switch str(obj["type"]) {
		case "content_block_delta":
			d, _ := obj["delta"].(map[string]any)
			switch str(d["type"]) {
			case "text_delta", "thinking_delta", "input_json_delta":
				return true, nil
			}
		case "content_block_start":
			cb, _ := obj["content_block"].(map[string]any)
			if t := str(cb["type"]); t == "text" || t == "tool_use" {
				return true, nil
			}
		}
	}

	if choices, ok := obj["choices"].([]any); ok {
		for _, raw := range choices {
			choice, _ := raw.(map[string]any)
			if d, ok := choice["delta"].(map[string]any); ok && semanticDelta(d) {
				return true, nil
			}
			if m, ok := choice["message"].(map[string]any); ok && semanticDelta(m) {
				return true, nil
			}
		}
	}

	typ := str(obj["type"])
	if strings.HasSuffix(typ, ".delta") || typ == "response.output_item.added" || typ == "response.content_part.added" {
		return true, nil
	}
	return false, nil
}

func semanticDelta(d map[string]any) bool {
	for _, k := range []string{"content", "reasoning_content", "reasoning", "tool_calls"} {
		v, ok := d[k]
		if !ok || v == nil {
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			continue
		}
		if a, ok := v.([]any); ok && len(a) == 0 {
			continue
		}
		return true
	}
	return false
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
