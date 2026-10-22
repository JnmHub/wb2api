package responses

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"wb2api/internal/jsonx"
)

const heldCall = `<｜｜DSML｜｜ calls><｜｜DSML｜｜ invoke name="ns.tool"><｜｜DSML｜｜ parameter name="input" string="true">hello</｜｜DSML｜｜ parameter></｜｜DSML｜｜ invoke></｜｜DSML｜｜ calls>`

func contentChunk(piece string) *jsonx.Obj {
	return obj("choices", []any{obj("index", num(0), "delta", obj("content", piece))})
}

func textDeltas(t *testing.T, fs []Frame) string {
	t.Helper()
	var b strings.Builder
	for _, f := range fs {
		if !bytes.HasPrefix(f, []byte("event: response.output_text.delta\n")) {
			continue
		}
		data := bytes.SplitN(f, []byte("data: "), 2)[1]
		o, err := jsonx.UnmarshalObj(bytes.TrimSpace(data))
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString(o.Get("delta").(string))
	}
	return b.String()
}

func assertFinalText(t *testing.T, s *Stream, fs []Frame, want string) {
	t.Helper()
	if got := strings.Join(s.textParts, ""); got != want {
		t.Fatalf("retained text mismatch: got %d bytes, want %d", len(got), len(want))
	}
	for _, f := range fs {
		if bytes.HasPrefix(f, []byte("event: response.completed\n")) {
			o, err := jsonx.UnmarshalObj(bytes.TrimSpace(bytes.SplitN(f, []byte("data: "), 2)[1]))
			if err != nil {
				t.Fatal(err)
			}
			r, _ := asObj(o.Get("response"))
			if r.Get("output_text") != want {
				t.Fatal("completed output_text differs from streamed text")
			}
			return
		}
	}
	t.Fatal("missing completed event")
}

func TestDSMLUnclosedBounded(t *testing.T) {
	s := NewStream("m", Info{}, Env{})
	pieces := []string{"<DSML"}
	for range 70 {
		pieces = append(pieces, strings.Repeat("a", 4096))
	}
	var visible strings.Builder
	for _, piece := range pieces {
		visible.WriteString(textDeltas(t, s.Chunk(contentChunk(piece))))
		if s.textHold.buf.Len() > dsmlMaxHold || s.textHold.buf.Cap() > 2*dsmlMaxHold {
			t.Fatal("unbounded speculative hold")
		}
	}
	if visible.Len() == 0 {
		t.Fatal("unclosed DSML withheld all text before EOF")
	}
	end := s.Finish()
	visible.WriteString(textDeltas(t, end))
	want := strings.Join(pieces, "")
	if visible.String() != want {
		t.Fatal("fallback lost or reordered text")
	}
	assertFinalText(t, s, end, want)
}

func TestDSMLBudgetFallbackDoesNotReparse(t *testing.T) {
	for _, step := range []int{1, 4096, 2 * dsmlMaxHold} {
		t.Run(fmt.Sprint(step), func(t *testing.T) {
			s := NewStream("m", Info{}, Env{})
			// The closing tag arrives too late. Even at EOF, text already
			// released by the hold budget must not be reinterpreted as a call.
			body := strings.Replace(heldCall, "hello", strings.Repeat("x", dsmlMaxHold), 1)
			var visible strings.Builder
			for i := 0; i < len(body); i += step {
				visible.WriteString(textDeltas(t, s.Chunk(contentChunk(body[i:min(i+step, len(body))]))))
			}
			end := s.Finish()
			visible.WriteString(textDeltas(t, end))
			if visible.String() != body || len(s.dsmlCalls) != 0 {
				t.Fatal("over-budget call changed already-emitted literal text")
			}
			assertFinalText(t, s, end, body)
		})
	}
}

func TestDSMLSplitValidLateCall(t *testing.T) {
	for _, step := range []int{1, 7, len(heldCall)} {
		t.Run(fmt.Sprint(step), func(t *testing.T) {
			s := NewStream("m", Info{CustomNames: map[string]bool{"ns.tool": true}, Namespaces: map[string]string{"ns.tool": "ns"}}, Env{})
			prefix := strings.Repeat("ordinary text ", 30000)
			visible := textDeltas(t, s.Chunk(contentChunk(prefix)))
			// Split only on rune boundaries: upstream JSON strings are valid UTF-8.
			runes := []rune(heldCall)
			for i := 0; i < len(runes); i += step {
				visible += textDeltas(t, s.Chunk(contentChunk(string(runes[i:min(i+step, len(runes))]))))
			}
			visible += textDeltas(t, s.Chunk(contentChunk(" after")))
			end := s.Finish()
			visible += textDeltas(t, end)
			if visible != prefix+" after" || len(s.dsmlCalls) != 1 {
				t.Fatal("split DSML changed surrounding text or lost call")
			}
			var call *jsonx.Obj
			for _, o := range s.outputs {
				if o != nil && o.Get("type") == "custom_tool_call" {
					call = o
				}
			}
			if call == nil || call.Get("input") != "hello" || call.Get("namespace") != "ns" {
				t.Fatalf("custom/namespace conversion lost: %v", call)
			}
			assertFinalText(t, s, end, visible)
		})
	}
}

func TestDSMLNormalAndEOF(t *testing.T) {
	for _, body := range []string{"normal text", "a < b > c", "<", "<DSML", "<DSML calls>unclosed", "hello <tag>world</tag>", "中文<DSML" + strings.Repeat("界", dsmlMaxHold/3)} {
		t.Run(fmt.Sprint(len(body)), func(t *testing.T) {
			s := NewStream("m", Info{}, Env{})
			visible := textDeltas(t, s.Chunk(contentChunk(body)))
			end := s.Finish()
			visible += textDeltas(t, end)
			if visible != body || !utf8.ValidString(visible) {
				t.Fatal("EOF/text fallback lost bytes or split UTF-8")
			}
			assertFinalText(t, s, end, body)
		})
	}
}

func TestDSMLValidLargeCall(t *testing.T) {
	value := strings.Repeat("v", dsmlMaxHold-4096)
	body := strings.Replace(heldCall, "hello", value, 1)
	s := NewStream("m", Info{}, Env{})
	for i := 0; i < len(body); i += 4096 {
		s.Chunk(contentChunk(body[i:min(i+4096, len(body))]))
	}
	s.Finish()
	if len(s.dsmlCalls) != 1 {
		t.Fatal("within-budget large DSML call lost")
	}
	args, err := jsonx.UnmarshalObj([]byte(s.dsmlCalls[0].args))
	if err != nil || args.Get("input") != value {
		t.Fatal("large DSML argument corrupted")
	}
}

func TestDSMLStructuredArgumentsUnaffected(t *testing.T) {
	value := strings.Repeat("v", 2*dsmlMaxHold)
	args := `{"input":"` + value + `"}`
	s := NewStream("m", Info{}, Env{})
	for i := 0; i < len(args); i += 4096 {
		s.Chunk(obj("choices", []any{obj("delta", obj("tool_calls", []any{obj("index", num(0), "id", "call", "function", obj("name", "tool", "arguments", args[i:min(i+4096, len(args))]))}))}))
	}
	s.SawDone()
	s.Finish()
	if s.tools[0].arguments.String() != args {
		t.Fatal("DSML hold budget affected structured arguments")
	}
}

func TestDSMLTimeBudgetOnNextChunk(t *testing.T) {
	s := NewStream("m", Info{}, Env{})
	s.Chunk(contentChunk("<DSML"))
	// No sleep: directly age the converter's monotonic hold timestamp.
	s.textHold.since = time.Now().Add(-dsmlMaxHoldTime - time.Second)
	if got := textDeltas(t, s.Chunk(jsonx.NewObj())); got != "<DSML" {
		t.Fatalf("usage/empty chunk failed to release aged hold: %q", got)
	}
	if s.textHold.buf.Len() != 0 {
		t.Fatal("aged hold retained")
	}
}

func BenchmarkDSMLUnclosed(b *testing.B) {
	for _, size := range []int{32 << 10, 256 << 10, 1 << 20} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			piece := contentChunk(strings.Repeat("x", 4096))
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				s := NewStream("m", Info{}, Env{})
				s.Chunk(contentChunk("<DSML"))
				for fed := 0; fed < size; fed += 4096 {
					s.Chunk(piece)
				}
			}
		})
	}
}
