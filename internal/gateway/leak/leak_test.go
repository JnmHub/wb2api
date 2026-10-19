package leak

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

type goldCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func readJSON(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

func norm(cs []Call) []goldCall {
	if cs == nil {
		return nil
	}
	out := make([]goldCall, len(cs))
	for i, c := range cs {
		out[i] = goldCall{c.Name, c.Arguments}
	}
	return out
}

func TestParseGolden(t *testing.T) {
	var cases []struct {
		Text          string     `json:"text"`
		Leaked        []goldCall `json:"leaked"`
		LeakedClean   string     `json:"leaked_clean"`
		DSML          []goldCall `json:"dsml"`
		DSMLClean     string     `json:"dsml_clean"`
		RepairChanged bool       `json:"repair_changed"`
		RepairContent string     `json:"repair_content"`
		RepairCalls   []goldCall `json:"repair_calls"`
	}
	readJSON(t, "parse.json", &cases)
	if len(cases) < 100 {
		t.Fatalf("too few vectors: %d", len(cases))
	}
	recovered := 0
	for i, c := range cases {
		lc, lclean, lok := ParseLeaked(c.Text)
		if lok != (c.Leaked != nil) || lclean != c.LeakedClean || !reflect.DeepEqual(norm(lc), c.Leaked) {
			t.Errorf("#%d ParseLeaked(%q)\n got  %v %q\n want %v %q", i, c.Text, norm(lc), lclean, c.Leaked, c.LeakedClean)
		}
		if lok {
			recovered++
		}
		dc, dclean, dok := ParseDSML(c.Text)
		if dok != (c.DSML != nil) || dclean != c.DSMLClean || (dok && !reflect.DeepEqual(norm(dc), c.DSML)) {
			t.Errorf("#%d ParseDSML(%q)\n got  %v %q\n want %v %q", i, c.Text, norm(dc), dclean, c.DSML, c.DSMLClean)
		}
		rc, rclean, rok := RepairText(c.Text)
		if rok != c.RepairChanged {
			t.Errorf("#%d RepairText changed=%v want %v for %q", i, rok, c.RepairChanged, c.Text)
		} else if rok {
			if rclean != c.RepairContent || !reflect.DeepEqual(norm(rc), c.RepairCalls) {
				t.Errorf("#%d RepairText(%q)\n got  %v %q\n want %v %q", i, c.Text, norm(rc), rclean, c.RepairCalls, c.RepairContent)
			}
		}
	}
	if recovered < 10 {
		t.Errorf("corpus should exercise recovery; only %d recovered", recovered)
	}
}

type goldAction struct {
	K string          `json:"k"`
	V json.RawMessage `json:"v"`
}

func TestLegacyFilterGolden(t *testing.T) {
	var cases []struct {
		Chunks []string       `json:"chunks"`
		Steps  [][]goldAction `json:"steps"`
	}
	readJSON(t, "filter.json", &cases)
	held := 0
	for i, c := range cases {
		f := NewFilter(Legacy)
		for si := 0; si <= len(c.Chunks); si++ {
			var got []Action
			if si < len(c.Chunks) {
				got = f.Feed(c.Chunks[si])
			} else {
				got = f.Flush()
			}
			want := c.Steps[si]
			if len(got) != len(want) {
				t.Fatalf("#%d step %d: got %d actions %v, want %d %v (chunks %q)", i, si, len(got), got, len(want), want, c.Chunks)
			}
			for ai, a := range got {
				w := want[ai]
				switch w.K {
				case "text":
					var s string
					_ = json.Unmarshal(w.V, &s)
					if a.Kind != Text || a.Text != s {
						t.Fatalf("#%d step %d action %d: got %+v want text %q (chunks %q)", i, si, ai, a, s, c.Chunks)
					}
				case "calls":
					var cs []goldCall
					_ = json.Unmarshal(w.V, &cs)
					if a.Kind != Calls || !reflect.DeepEqual(norm(a.Calls), cs) {
						t.Fatalf("#%d step %d action %d: got %+v want calls %v (chunks %q)", i, si, ai, a, cs, c.Chunks)
					}
					held++
				}
			}
		}
	}
	if held < 5 {
		t.Errorf("corpus should exercise held-call conversion; only %d", held)
	}
}

func collect(f *Filter, chunks ...string) (text string, calls []Call, perFeed [][]Action) {
	for _, c := range chunks {
		acts := f.Feed(c)
		perFeed = append(perFeed, acts)
		for _, a := range acts {
			if a.Kind == Text {
				text += a.Text
			} else {
				calls = append(calls, a.Calls...)
			}
		}
	}
	for _, a := range f.Flush() {
		if a.Kind == Text {
			text += a.Text
		} else {
			calls = append(calls, a.Calls...)
		}
	}
	return
}

// The stall seen in production: with the legacy policy nothing after
// "<thinking>" is forwarded until the stream ends.
func TestLegacyStallVsPrecise(t *testing.T) {
	for _, tag := range []string{"<thinking>", "<think>", "<args>", "<params>"} {
		leg := NewFilter(Legacy)
		pre := NewFilter(Precise)
		chunks := []string{"intro ", tag}
		for i := 0; i < 200; i++ {
			chunks = append(chunks, "more text ")
		}
		_, _, legSteps := collect(leg, chunks...)
		_, _, preSteps := collect(pre, chunks...)
		legEmitted, preEmitted := 0, 0
		for _, st := range legSteps {
			legEmitted += len(st)
		}
		for _, st := range preSteps {
			preEmitted += len(st)
		}
		if legEmitted != 1 { // only "intro "
			t.Errorf("%s: legacy should withhold everything after the tag, emitted %d actions", tag, legEmitted)
		}
		if preEmitted < len(chunks)-1 {
			t.Errorf("%s: precise should keep streaming, emitted %d of %d", tag, preEmitted, len(chunks))
		}
		if pre.Held() != 0 {
			t.Errorf("%s: precise filter still holds %d bytes", tag, pre.Held())
		}
	}
}

// Everything the precise policy streams plus what it recovers must equal the
// complete original text when no call is present.
func TestPreciseNeverLosesText(t *testing.T) {
	inputs := []string{
		"a <think>x</think> b <args>y</args> List<String> c < d",
		"<thinking>deep</thinking> done",
		"1 <2 and 3> 4 <param",
		"<div><span>hi</span></div>",
	}
	for _, in := range inputs {
		for step := 1; step <= 5; step++ {
			f := NewFilter(Precise)
			var chunks []string
			for i := 0; i < len(in); i += step {
				end := i + step
				if end > len(in) {
					end = len(in)
				}
				chunks = append(chunks, in[i:end])
			}
			text, calls, _ := collect(f, chunks...)
			if text != in || len(calls) != 0 {
				t.Errorf("step %d: %q -> %q (%d calls)", step, in, text, len(calls))
			}
		}
	}
}

// Real printed calls are still recovered in precise mode, in every chunking.
func TestPreciseRecoversPrintedCalls(t *testing.T) {
	const hybrid = "Let me run it.\n<run_code><parameter name=\"code\">print(1)</parameter>\n" +
		"<parameter name=\"description\">say hi</｜｜DSML｜｜ parameter>\n</run_code>"
	const dsml = "ok <｜｜DSML｜｜ calls><｜｜DSML｜｜ invoke name=\"run_shell\">" +
		"<｜｜DSML｜｜ parameter name=\"command\" string=\"true\">ls -l</｜｜DSML｜｜ parameter>" +
		"</｜｜DSML｜｜ invoke></｜｜DSML｜｜ calls>"
	runes := func(s string) []string {
		var out []string
		for _, r := range s {
			out = append(out, string(r))
		}
		return out
	}
	for name, in := range map[string]string{"hybrid": hybrid, "dsml": dsml} {
		for _, chunks := range [][]string{{in}, runes(in)} {
			for _, mode := range []Mode{Legacy, Precise} {
				if name == "dsml" && mode == Legacy {
					// Known legacy quirk (kept for parity): its case-sensitive tag regex
					// runs on lower-cased text, so a finished DSML opening tag is never
					// withheld and the printed call is forwarded as prose.
					continue
				}
				text, calls, _ := collect(NewFilter(mode), chunks...)
				if len(calls) != 1 {
					t.Fatalf("%s mode %d chunks %d: want 1 call, got %d (text %q)", name, mode, len(chunks), len(calls), text)
				}
				if strings.Contains(text, "<run_code") || strings.Contains(text, "DSML") || strings.Contains(text, "<parameter") {
					t.Errorf("%s mode %d: markup leaked into text %q", name, mode, text)
				}
			}
		}
	}
}

func TestMaxHoldReleases(t *testing.T) {
	f := NewFilter(Precise)
	f.MaxHold = 100
	f.Feed("<run_code>")
	var out []Action
	for i := 0; i < 20 && len(out) == 0; i++ {
		out = f.Feed("xxxxxxxxxx")
	}
	if len(out) != 1 || out[0].Kind != Text || f.Held() != 0 {
		t.Fatalf("expected release past MaxHold, got %+v held=%d", out, f.Held())
	}
}

// Feeding must be linear: the legacy filter was quadratic while holding.
func TestFeedIsLinearWhileHolding(t *testing.T) {
	run := func(n int) time.Duration {
		f := NewFilter(Legacy)
		f.Feed("<thinking>")
		start := time.Now()
		for i := 0; i < n; i++ {
			f.Feed("more text here ")
		}
		return time.Since(start)
	}
	small, large := run(2000), run(16000)
	// 8x the input must cost well under 64x (quadratic) the time.
	if small > 0 && large > small*40 && large > 50*time.Millisecond {
		t.Errorf("held-buffer feed looks super-linear: %v for 2k chunks, %v for 16k", small, large)
	}
}

func BenchmarkFeedProse(b *testing.B) {
	f := NewFilter(Precise)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		f.Feed("hello world ")
	}
}

func BenchmarkFeedHeld(b *testing.B) {
	f := NewFilter(Legacy)
	f.MaxHold = 1 << 30
	f.Feed("<thinking>")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		f.Feed("more text here ")
	}
}
