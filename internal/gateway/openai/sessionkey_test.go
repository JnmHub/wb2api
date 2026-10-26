package openai

import (
	"net/http/httptest"
	"testing"

	"wb2api/internal/jsonx"
)

// A sticky key can arrive as a header, in the payload, in metadata or as the
// prompt cache hint; the header wins because it is the most explicit.
func TestExtractSessionKeySources(t *testing.T) {
	cases := []struct {
		name    string
		header  map[string]string
		payload string
		want    string
	}{
		{"conversation header", map[string]string{"X-Conversation-Id": "h1"}, `{}`, "h1"},
		{"wb session header", map[string]string{"X-Wb-Session-Id": "h2"}, `{}`, "h2"},
		{"session id in payload", nil, `{"session_id":"p1"}`, "p1"},
		{"prompt cache hint", nil, `{"prompt_cache_key":"cache-42"}`, "cache-42"},
		{"metadata hint", nil, `{"metadata":{"prompt_cache_key":"m1"}}`, "m1"},
		{"header beats payload", map[string]string{"Session-Id": "h3"}, `{"session_id":"p2"}`, "h3"},
		{"nothing", nil, `{}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			for k, v := range c.header {
				r.Header.Set(k, v)
			}
			obj, err := jsonx.UnmarshalObj([]byte(c.payload))
			if err != nil {
				t.Fatal(err)
			}
			if got := extractSessionKey(r, obj); got != c.want {
				t.Fatalf("extractSessionKey = %q; want %q", got, c.want)
			}
		})
	}
}
