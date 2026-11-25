package codearts

import (
	"bytes"
	"testing"
	"unicode/utf8"

	"wb2api/internal/jsonx"
)

func TestPrepareBodyWithHistoryLimitKeepsNewestTurns(t *testing.T) {
	const fixture = `{
		"messages":[
			{"role":"system","content":"系统说明"},
			{"role":"user","content":"第一轮 😀\n含\"引号\"和反斜杠\\。"},
			{"role":"assistant","content":"旧答复"},
			{"role":"developer","content":"中途更新的说明"},
			{"role":"user","content":"第二轮资料"},
			{"role":"assistant","content":"第二轮答复"},
			{"role":"user","content":"当前轮"},
			{"role":"assistant","tool_calls":[{"id":"call_active","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_active","content":"当前工具结果"}
		],
		"tools":[{"type":"function","function":{"name":"lookup","description":"查资料","parameters":{"type":"object"}}}],
		"max_tokens":1024
	}`
	const model, chatID = "deepseek-v4.1-flash", "test-session"
	input := newObj(t, fixture)
	original, err := jsonx.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	full, _, err := PrepareBody(input, model, chatID)
	if err != nil {
		t.Fatal(err)
	}
	messages, _ := input.Arr("messages")
	retained := input.Clone()
	retained.Set("messages", append([]any{messages[0]}, messages[3:]...))
	trimmed, _, err := PrepareBody(retained, model, chatID)
	if err != nil {
		t.Fatal(err)
	}
	minimum := input.Clone()
	minimum.Set("messages", append([]any{messages[0], messages[3]}, messages[6:]...))
	active, _, err := PrepareBody(minimum, model, chatID)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name  string
		limit int
		want  []byte
	}{
		{name: "disabled", limit: 0, want: full},
		{name: "full body fits exactly", limit: len(full), want: full},
		{name: "one byte above limit removes oldest turn", limit: len(full) - 1, want: trimmed},
		{name: "trimmed body fits exactly", limit: len(trimmed), want: trimmed},
		{name: "trimmed body still too large", limit: len(trimmed) - 1, want: active},
		{name: "active turn and tool definitions exceed cap", limit: len(active) - 1, want: active},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, _, budget, err := PrepareBodyWithHistoryLimit(input, model, chatID, test.limit)
			if err != nil {
				t.Fatal(err)
			}
			if !utf8.Valid(body) || !bytes.Equal(body, test.want) {
				t.Fatalf("body = %s; want %s", body, test.want)
			}
			if budget != 1024 {
				t.Fatalf("output token budget = %d; want 1024", budget)
			}
		})
	}
	after, err := jsonx.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("history trimming changed the caller's original request")
	}
}

func TestPrepareBodyWithHistoryLimitDoesNotSplitToolChains(t *testing.T) {
	const model = "deepseek-v4.1-flash"
	for _, test := range []struct {
		name, call, result string
	}{
		{
			name:   "parallel Chat tools",
			call:   `{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"lookup","arguments":"{}"}},{"id":"call_b","type":"function","function":{"name":"lookup","arguments":"{}"}}]}`,
			result: `{"role":"tool","tool_call_id":"call_a","content":"A"},{"role":"tool","tool_call_id":"call_b","content":"B"}`,
		},
		{
			name:   "mixed Messages tool result and text",
			call:   `{"role":"assistant","content":[{"type":"tool_use","id":"use","name":"lookup","input":{}}]}`,
			result: `{"role":"user","content":[{"type":"tool_result","tool_use_id":"use","content":"found"},{"type":"text","text":"Use that result"}]}`,
		},
		{
			name:   "legacy function result",
			call:   `{"role":"assistant","function_call":{"name":"lookup","arguments":"{}"}}`,
			result: `{"role":"function","name":"lookup","content":"found"}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			const earlier = `{"role":"user","content":"Earlier turn"},`
			const boundary = `{"role":"user","content":"Turn before delayed tool result"},`
			const current = `{"role":"user","content":"Latest turn"}`
			const prefix = `{"messages":[`
			const suffix = `]}`
			truncated := newObj(t, prefix+boundary+test.result+suffix)
			unsafe, _, err := PrepareBody(truncated, model, "")
			if err != nil {
				t.Fatal(err)
			}

			t.Run("retain the call and result when there is no safe cut", func(t *testing.T) {
				input := newObj(t, prefix+earlier+test.call+","+boundary+test.result+suffix)
				want, _, err := PrepareBody(input, model, "")
				if err != nil {
					t.Fatal(err)
				}
				body, _, _, removed, err := PrepareBodyWithHistoryAccounting(input, model, "", len(unsafe))
				if err != nil || removed != 0 || !bytes.Equal(body, want) {
					t.Fatalf("body = %s, removed tokens = %d, error = %v; want %s and no removed tokens", body, removed, err, want)
				}
			})

			t.Run("use a later cut after the tool result", func(t *testing.T) {
				const reply = ",{\"role\":\"assistant\",\"content\":\"Used the result\"},"
				input := newObj(t, prefix+earlier+test.call+","+boundary+test.result+reply+current+suffix)
				retained := newObj(t, prefix+current+suffix)
				want, _, err := PrepareBody(retained, model, "")
				if err != nil {
					t.Fatal(err)
				}
				unsafeWithCurrent, _, err := PrepareBody(newObj(t, prefix+boundary+test.result+","+current+suffix), model, "")
				if err != nil {
					t.Fatal(err)
				}
				body, _, _, err := PrepareBodyWithHistoryLimit(input, model, "", len(unsafeWithCurrent))
				if err != nil || !bytes.Equal(body, want) {
					t.Fatalf("body = %s, error = %v; want %s", body, err, want)
				}
			})
		})
	}
}

func TestPrepareBodyWithHistoryLimitKeepsActiveFollowUp(t *testing.T) {
	const prefix = "{\"messages\":["
	const suffix = "]}"
	const older = "{\"role\":\"user\",\"content\":\"old prompt\"},{\"role\":\"assistant\",\"content\":\"old reply\"},"
	for _, latest := range []string{
		"{\"role\":\"user\",\"content\":\"First half\"},{\"role\":\"user\",\"content\":\"Second half\"}",
		"{\"role\":\"user\",\"content\":\"Look up a file\"}," +
			"{\"role\":\"assistant\",\"tool_calls\":[{\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]}," +
			"{\"role\":\"tool\",\"tool_call_id\":\"c1\",\"content\":\"found\"},{\"role\":\"user\",\"content\":\"Use this result\"}",
	} {
		active := newObj(t, prefix+latest+suffix)
		want, _, err := PrepareBody(active, "deepseek-v4.1-flash", "")
		if err != nil {
			t.Fatal(err)
		}
		input := newObj(t, prefix+older+latest+suffix)
		body, _, _, err := PrepareBodyWithHistoryLimit(input, "deepseek-v4.1-flash", "", len(want))
		if err != nil || !bytes.Equal(body, want) {
			t.Fatalf("body = %s, error = %v; want %s", body, err, want)
		}
		body, _, _, err = PrepareBodyWithHistoryLimit(input, "deepseek-v4.1-flash", "", len(want)-1)
		if err != nil || !bytes.Equal(body, want) {
			t.Fatalf("body = %s, error = %v; want %s", body, err, want)
		}
	}
}

func TestPrepareBodyWithHistoryLimitTrimsAssistantFirstHistory(t *testing.T) {
	const model = "deepseek-v4.1-flash"
	const prefix = "{\"messages\":["
	const system = "{\"role\":\"system\",\"content\":\"Keep system prompt\"},"
	const old = "{\"role\":\"assistant\",\"content\":\"Earlier answer\"},"
	const current = "{\"role\":\"user\",\"content\":\"Current prompt\"}]}"
	retained, _, err := PrepareBody(newObj(t, prefix+system+current), model, "")
	if err != nil {
		t.Fatal(err)
	}
	body, _, _, err := PrepareBodyWithHistoryLimit(newObj(t, prefix+system+old+current), model, "", len(retained))
	if err != nil || !bytes.Equal(body, retained) {
		t.Fatalf("body = %s, error = %v; want %s", body, err, retained)
	}
}

func TestPrepareBodyWithHistoryAccountingCountsOnlyRemovedTurns(t *testing.T) {
	const model = "deepseek-v4.1-flash"
	const system = `{"role":"system","content":"系统说明"},`
	const developer = `{"role":"developer","content":"开发说明"},`
	const old = `{"role":"user","content":[{"type":"text","text":"旧问题"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}}]},
		{"role":"assistant","content":"旧答复"},`
	const current = `{"role":"user","content":"当前轮"}`
	const begin = `{"messages":[`
	const end = `]}`
	input := newObj(t, begin+system+old+developer+current+end)
	want, _, err := PrepareBody(newObj(t, begin+system+developer+current+end), model, "")
	if err != nil {
		t.Fatal(err)
	}
	body, _, _, removed, err := PrepareBodyWithHistoryAccounting(input, model, "", 1)
	// Each removed message has three Han characters and three framing tokens.
	if err != nil || !bytes.Equal(body, want) || removed != 12 {
		t.Fatalf("body = %s, removed tokens = %d, error = %v; want %s and 12 removed tokens", body, removed, err, want)
	}
}
