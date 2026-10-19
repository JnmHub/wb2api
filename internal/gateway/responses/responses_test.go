package responses

import (
	"bytes"
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"wb2api/internal/catalog"
	"wb2api/internal/jsonx"
)

// Vectors come from the legacy Python implementation
// (py -I tools/genvectors/gen_responses.py); ids are a per-prefix counter and
// the clock is frozen at 1790000000.

type vectors struct {
	ToChat []struct {
		Payload    json.RawMessage   `json:"payload"`
		Preserve   bool              `json:"preserve"`
		Chat       string            `json:"chat"`
		NS         map[string]string `json:"ns"`
		Custom     []string          `json:"custom"`
		Tools      json.RawMessage   `json:"tools"`
		HasNS      bool              `json:"has_ns"`
		ToolChoice json.RawMessage   `json:"tool_choice"`
		Parallel   json.RawMessage   `json:"parallel"`
	} `json:"to_chat"`
	NonStream []struct {
		Chat       json.RawMessage   `json:"chat"`
		Model      string            `json:"model"`
		Custom     []string          `json:"custom"`
		Tools      json.RawMessage   `json:"tools"`
		NS         map[string]string `json:"ns"`
		ToolChoice json.RawMessage   `json:"tool_choice"`
		Parallel   json.RawMessage   `json:"parallel"`
		Want       string            `json:"want"`
	} `json:"nonstream"`
	Stream []struct {
		Chunks     []json.RawMessage `json:"chunks"`
		Raw        []string          `json:"raw"`
		Done       bool              `json:"done"`
		Custom     []string          `json:"custom"`
		NS         map[string]string `json:"ns"`
		Tools      json.RawMessage   `json:"tools"`
		ToolChoice json.RawMessage   `json:"tool_choice"`
		Parallel   json.RawMessage   `json:"parallel"`
		Model      string            `json:"model"`
		Want       string            `json:"want"`
		Usage      json.RawMessage   `json:"usage"`
	} `json:"stream"`
}

func load(t *testing.T) *vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/responses.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return &v
}

func parse(t *testing.T, raw []byte) any {
	t.Helper()
	v, err := jsonx.Unmarshal(raw)
	if err != nil {
		t.Fatalf("%v in %s", err, raw)
	}
	return v
}

func enc(t *testing.T, v any) string {
	t.Helper()
	b, err := jsonx.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type counterEnv struct{ n map[string]int }

func newEnv() (Env, *counterEnv) {
	c := &counterEnv{n: map[string]int{}}
	return Env{
		NewID: func(prefix string) string { c.n[prefix]++; return prefix + strconv.Itoa(c.n[prefix]) },
		Now:   func() int64 { return 1790000000 },
	}, c
}

func keysOf(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestToChat(t *testing.T) {
	cat := &catalog.Service{}
	for i, c := range load(t).ToChat {
		payload := parse(t, c.Payload).(*jsonx.Obj)
		chat, info := ToChat(payload, Options{PreserveContract: c.Preserve, MaxOutputTokens: cat.MaxOutputTokens})
		if got := enc(t, chat); got != c.Chat {
			t.Errorf("case %d (preserve=%v) chat differs\n got %s\nwant %s", i, c.Preserve, got, c.Chat)
			continue
		}
		if strings.Join(keysOf(info.CustomNames), ",") != strings.Join(c.Custom, ",") {
			t.Errorf("case %d custom names %v want %v", i, keysOf(info.CustomNames), c.Custom)
		}
		if len(info.Namespaces) != len(c.NS) {
			t.Errorf("case %d namespaces %v want %v", i, info.Namespaces, c.NS)
		}
		for k, v := range c.NS {
			if info.Namespaces[k] != v {
				t.Errorf("case %d namespace[%s] = %q want %q", i, k, info.Namespaces[k], v)
			}
		}
		if got, want := enc(t, orEmpty(info.Tools)), enc(t, parse(t, c.Tools)); got != want {
			t.Errorf("case %d echoed tools\n got %s\nwant %s", i, got, want)
		}
		if got, want := enc(t, info.ToolChoice), enc(t, parse(t, c.ToolChoice)); got != want {
			t.Errorf("case %d tool_choice %s want %s", i, got, want)
		}
		if got, want := enc(t, info.ParallelToolCalls), enc(t, parse(t, c.Parallel)); got != want {
			t.Errorf("case %d parallel_tool_calls %s want %s", i, got, want)
		}
	}
}

func orEmpty(l []any) []any {
	if l == nil {
		return []any{}
	}
	return l
}

func infoOf(t *testing.T, custom []string, tools, toolChoice, parallel json.RawMessage, ns map[string]string) Info {
	t.Helper()
	info := Info{CustomNames: map[string]bool{}, Namespaces: ns,
		ToolChoice: parse(t, toolChoice), ParallelToolCalls: parse(t, parallel)}
	for _, n := range custom {
		info.CustomNames[n] = true
	}
	info.Tools, _ = parse(t, tools).([]any)
	if len(info.Tools) == 0 {
		info.Tools = nil
	}
	return info
}

func TestChatToResponse(t *testing.T) {
	for i, c := range load(t).NonStream {
		env, _ := newEnv()
		info := infoOf(t, c.Custom, c.Tools, c.ToolChoice, c.Parallel, c.NS)
		chat := parse(t, c.Chat).(*jsonx.Obj)
		got := enc(t, ChatToResponse(chat, c.Model, info, env))
		if got != c.Want {
			t.Errorf("case %d\n got %s\nwant %s", i, got, c.Want)
		}
	}
}

func TestStream(t *testing.T) {
	for i, c := range load(t).Stream {
		env, _ := newEnv()
		info := infoOf(t, c.Custom, c.Tools, c.ToolChoice, c.Parallel, c.NS)
		s := NewStream(c.Model, info, env)
		var out bytes.Buffer
		write := func(fs []Frame) {
			for _, f := range fs {
				out.Write(f)
			}
		}
		write(s.Start())
		for _, ch := range c.Chunks {
			write(s.Line([]byte("data: " + string(ch) + "\n")))
			write(s.Line([]byte("\n")))
		}
		for _, r := range c.Raw {
			write(s.Line([]byte(r)))
		}
		if c.Done {
			write(s.Line([]byte("data: [DONE]\n")))
		}
		write(s.Finish())
		// The legacy frame cleaner appends a stray blank line to every frame it
		// rewrites; that is the one deliberate difference.
		want := strings.ReplaceAll(c.Want, "\n\n\n", "\n\n")
		if got := out.String(); got != want {
			t.Errorf("case %d differs\n got %s\nwant %s", i, firstDiff(got, want), want[:min(len(want), 600)])
			continue
		}
		wantUsage := parse(t, c.Usage)
		if got := s.Usage(); (got == nil) != (wantUsage == nil) {
			t.Errorf("case %d usage presence: got %v want %v", i, got, wantUsage)
		}
	}
}

// firstDiff shows where two streams part ways.
func firstDiff(got, want string) string {
	n := 0
	for n < len(got) && n < len(want) && got[n] == want[n] {
		n++
	}
	lo := max(0, n-80)
	return "…" + got[lo:min(len(got), n+200)] + "\n  want …" + want[lo:min(len(want), n+200)]
}
