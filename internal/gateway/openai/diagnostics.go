package openai

import (
	"encoding/json"
	"net/http"
	"regexp"

	"wb2api/internal/jsonx"
)

// upstreamCodeRe matches the business codes the vendors put in their bodies:
// InferHub.001001005.400, TM.00001041, APIG.0301, or a bare numeric code.
var upstreamCodeRe = regexp.MustCompile(`(?:InferHub|TM|APIG)\.[0-9A-Za-z.]+`)

// upstreamCodeOf extracts the upstream business code from a raw error body.
//
// The usage ledger used to keep only the HTTP status, so every rejected request
// looked identical ("Bad Request") and a screenshot of InferHub.001001005.400
// could not be matched to a record. The code is a vendor identifier, not user
// content, so it is safe to store.
func upstreamCodeOf(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if m := upstreamCodeRe.Find(body); len(m) > 0 {
		return string(m)
	}
	var doc struct {
		Code any `json:"error_code"`
	}
	if err := json.Unmarshal(body, &doc); err == nil && doc.Code != nil {
		if s, ok := doc.Code.(string); ok && s != "" && s != "0" {
			return s
		}
	}
	return ""
}

// requestShapeOf summarises the outbound body: counts and flags only, never
// message text, tool arguments, headers or credentials.
func requestShapeOf(shaped *jsonx.Obj, body []byte) *RequestShape {
	out := &RequestShape{Bytes: len(body), Stream: true}
	if shaped == nil {
		return out
	}
	if v, ok := shaped.Lookup("messages"); ok {
		if list, ok := v.([]any); ok {
			out.Messages = len(list)
		}
	}
	if v, ok := shaped.Lookup("tools"); ok {
		if list, ok := v.([]any); ok {
			out.Tools = len(list)
			for _, t := range list {
				if name := toolNameOf(t); len([]rune(name)) > 64 {
					out.LongToolNames++
				}
			}
		}
	}
	if v, ok := shaped.Lookup("tool_choice"); ok && v != nil {
		switch t := v.(type) {
		case string:
			out.HasToolChoice = true
			out.ToolChoice = t
		case *jsonx.Obj:
			out.HasToolChoice = true
			out.ToolChoice = "object"
		case map[string]any:
			out.HasToolChoice = true
			out.ToolChoice = "object"
		}
	}
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if v, ok := shaped.Lookup(key); ok {
			if n, ok := v.(float64); ok && n > 0 {
				out.MaxTokens = int64(n)
				break
			}
		}
	}
	return out
}

func toolNameOf(tool any) string {
	fn, ok := shapeField(tool, "function")
	if !ok {
		return ""
	}
	name, _ := shapeField(fn, "name")
	s, _ := name.(string)
	return s
}

// shapeField reads a key from either a decoded map or an ordered jsonx object.
func shapeField(v any, key string) (any, bool) {
	switch t := v.(type) {
	case map[string]any:
		x, ok := t[key]
		return x, ok
	case *jsonx.Obj:
		return t.Lookup(key)
	}
	return nil, false
}

// requestIDOf reads the id the gateway attached to this request.
func requestIDOf(r *http.Request) string {
	if r == nil {
		return ""
	}
	if v := r.Context().Value(requestIDKey{}); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

type requestIDKey struct{}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
