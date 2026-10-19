package webtools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"wb2api/internal/jsonx"
)

func TestWantsWebAndInstallDefs(t *testing.T) {
	tools := []any{
		map[string]any{"type": "web_search"},
		map[string]any{"type": "web_fetch"},
		map[string]any{"type": "function", "name": "get_time"},
	}
	search, fetch := WantsWeb(tools)
	if !search || !fetch {
		t.Fatalf("wants %v %v", search, fetch)
	}
	chat := jsonx.NewObj()
	var chatTools []any
	for _, raw := range tools {
		m := raw.(map[string]any)
		o := jsonx.NewObj()
		for k, v := range m {
			o.Set(k, v)
		}
		chatTools = append(chatTools, o)
	}
	chat.Set("tools", chatTools)
	InstallDefs(chat, true, true)
	list, _ := chat.Get("tools").([]any)
	var names []string
	for _, raw := range list {
		o, _ := raw.(*jsonx.Obj)
		if o == nil {
			continue
		}
		if fn, ok := o.Get("function").(*jsonx.Obj); ok {
			names = append(names, str(fn.Get("name")))
		} else {
			names = append(names, str(o.Get("name")))
		}
	}
	if strings.Join(names, ",") != "get_time,web_search,web_fetch" {
		t.Fatalf("installed tools: %v", names)
	}
}

func TestExecuteFormatsTheSearchResult(t *testing.T) {
	old := engines
	engines = []engine{{name: "fake", run: func(ctx context.Context, q string, n int, c *http.Client) engineResult {
		return engineResult{sources: []Source{{Title: "A", URL: "https://a.example/x", Snippet: "snip"}}, state: "ok", note: "Fake"}
	}}}
	defer func() { engines = old }()
	text := Execute(context.Background(), "web_search", `{"query":"golang","numResults":3}`)
	for _, want := range []string{"Search results for: golang", "[Fake]", "1. A", "https://a.example/x", "snip", "Cite the sources"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %q", want, text)
		}
	}
	short := Execute(context.Background(), "web_search", `{"query":"a"}`)
	if !strings.Contains(short, "at least 2 characters") {
		t.Errorf("short query: %q", short)
	}
	unknown := Execute(context.Background(), "nope", `{}`)
	if !strings.Contains(unknown, "not a tool this gateway runs") {
		t.Errorf("unknown: %q", unknown)
	}
}

func TestSearchEmptyAndFailureFormatting(t *testing.T) {
	old := engines
	defer func() { engines = old }()
	engines = []engine{{name: "fake", run: func(ctx context.Context, q string, n int, c *http.Client) engineResult {
		return engineResult{state: "ok"}
	}}}
	if got := Execute(context.Background(), "web_search", `{"query":"nothing here"}`); !strings.Contains(got, "No results found") {
		t.Fatalf("%q", got)
	}
	engines = []engine{{name: "dead", run: func(ctx context.Context, q string, n int, c *http.Client) engineResult {
		return engineResult{state: "error", detail: "boom"}
	}}}
	if got := Execute(context.Background(), "web_search", `{"query":"x y"}`); !strings.Contains(got, "temporarily unavailable") {
		t.Fatalf("%q", got)
	}
}

func TestFetchExtractsTextAndPaginates(t *testing.T) {
	old := guardFn
	guardFn = func(raw string) (*url.URL, error) { return url.Parse(raw) }
	defer func() { guardFn = old }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><head><script>bad()</script><style>x{}</style></head><body><h1>Title</h1><p>Hello &amp; world</p></body></html>`))
	}))
	defer srv.Close()
	out := Fetch(context.Background(), srv.URL, 0)
	for _, want := range []string{"URL: " + srv.URL, "Title", "Hello & world", "Characters: 0-"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	if strings.Contains(out, "bad()") || strings.Contains(out, "<h1>") {
		t.Errorf("markup leaked: %q", out)
	}
	past := Fetch(context.Background(), srv.URL, 100000)
	if !strings.Contains(past, "past the end") {
		t.Errorf("pagination: %q", past)
	}
}

func TestFetchRefusesNonTextAndPrivateHosts(t *testing.T) {
	old := guardFn
	guardFn = func(raw string) (*url.URL, error) { return url.Parse(raw) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte{0, 1, 2})
	}))
	defer srv.Close()
	if got := Fetch(context.Background(), srv.URL, 0); !strings.Contains(got, "not a text page") {
		t.Fatalf("%q", got)
	}
	guardFn = old
	for _, bad := range []string{"http://127.0.0.1/x", "http://localhost/x", "http://10.0.0.5/x", "ftp://example.com/x", "http://user:pass@example.com/"} {
		if _, err := guardURL(bad); err == nil {
			t.Errorf("%s should be refused", bad)
		}
	}
}

func TestSourcesFromResult(t *testing.T) {
	text := "Search results for: q\n\n1. First result\n   https://a.example/1\n   snippet\n\n2. Second\n   https://b.example/2"
	src := SourcesFromResult(text)
	if len(src) != 2 || src[0].URL != "https://a.example/1" || src[1].Title != "Second" {
		t.Fatalf("%+v", src)
	}
}
