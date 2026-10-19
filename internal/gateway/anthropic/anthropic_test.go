package anthropic

import (
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"testing"

	"wb2api/internal/catalog"
	"wb2api/internal/jsonx"
)

type vectors struct {
	ToChat []struct {
		Payload json.RawMessage `json:"payload"`
		Chat    string          `json:"chat"`
		Error   string          `json:"error"`
	} `json:"to_chat"`
	ToMessages []struct {
		Chat json.RawMessage `json:"chat"`
		Want string          `json:"want"`
	} `json:"to_messages"`
	Stream []struct {
		Chunks []json.RawMessage `json:"chunks"`
		Raw    []string          `json:"raw"`
		Want   string            `json:"want"`
		Usage  json.RawMessage   `json:"usage"`
	} `json:"stream"`
	Count []struct {
		Chat json.RawMessage `json:"chat"`
		Want int             `json:"want"`
	} `json:"count"`
}

func load(t *testing.T) *vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/anthropic.json")
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

func counterEnv() Env {
	n := map[string]int{}
	return Env{NewID: func(p string) string { n[p]++; return p + strconv.Itoa(n[p]) }}
}

func TestToChat(t *testing.T) {
	cat := &catalog.Service{}
	for i, c := range load(t).ToChat {
		payload := parse(t, c.Payload).(*jsonx.Obj)
		chat, err := ToChat(payload, Options{MaxOutputTokens: cat.MaxOutputTokens})
		if c.Error != "" {
			var inv *ErrInvalid
			if err == nil || !asInvalid(err, &inv) || inv.Msg != c.Error {
				t.Errorf("case %d: error %v, want %q", i, err, c.Error)
			}
			continue
		}
		if err != nil {
			t.Errorf("case %d: %v", i, err)
			continue
		}
		if got := enc(t, chat); got != c.Chat {
			t.Errorf("case %d differs\n got %s\nwant %s", i, got, c.Chat)
		}
	}
}

func asInvalid(err error, out **ErrInvalid) bool {
	e, ok := err.(*ErrInvalid)
	if ok {
		*out = e
	}
	return ok
}

func TestChatToMessages(t *testing.T) {
	for i, c := range load(t).ToMessages {
		chat, _ := parse(t, c.Chat).(*jsonx.Obj)
		got := enc(t, ChatToMessages(chat, counterEnv(), DefaultCached))
		if got != c.Want {
			t.Errorf("case %d\n got %s\nwant %s", i, got, c.Want)
		}
	}
}

func TestStream(t *testing.T) {
	for i, c := range load(t).Stream {
		s := NewStream("glm-5.3", counterEnv(), DefaultCached)
		var out bytes.Buffer
		write := func(fs []Frame) {
			for _, f := range fs {
				out.Write(f)
			}
		}
		for _, ch := range c.Chunks {
			write(s.Line([]byte("data: " + string(ch) + "\n")))
			write(s.Line([]byte("\n")))
		}
		for _, r := range c.Raw {
			write(s.Line([]byte(r)))
		}
		write(s.Line([]byte("data: [DONE]\n")))
		write(s.Finish())
		if out.String() != c.Want {
			t.Errorf("case %d differs\n got %s\nwant %s", i, out.String(), c.Want)
		}
		if (s.Usage() == nil) != (string(c.Usage) == "null") {
			t.Errorf("case %d usage %v want %s", i, s.Usage(), c.Usage)
		}
	}
}

func TestCountEstimate(t *testing.T) {
	for i, c := range load(t).Count {
		chat := parse(t, c.Chat).(*jsonx.Obj)
		if got := EstimateChatTokens(chat); got != c.Want {
			t.Errorf("case %d: %d want %d", i, got, c.Want)
		}
	}
}
