package openai

import (
	"context"
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

// Keep the original tool-name diagnostics and record the size and token budget
// of the final CodeArts payload after provider-specific normalization.
func requestShapeOf(shaped *jsonx.Obj, wire outbound) *RequestShape {
	out := &RequestShape{Bytes: len(wire.body), MaxTokens: wire.maxTokens, Stream: true}
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
	return requestIDFromContext(r.Context())
}

func requestIDFromContext(ctx context.Context) string {
	if m := metaFrom(ctx); m != nil {
		return m.id
	}
	return ""
}

// requestMeta travels with the request context: the streaming path and the
// post-hoc emitters only have a context, and a failure has to be traceable to
// the id the client was handed plus a summary of what was actually sent.
type requestMeta struct {
	id            string
	shape         *RequestShape
	codeArtsModel string
}

type requestMetaKey struct{}

func metaFrom(ctx context.Context) *requestMeta {
	if ctx == nil {
		return nil
	}
	m, _ := ctx.Value(requestMetaKey{}).(*requestMeta)
	return m
}

// setRequestShape records the outbound shape for this request.
func setRequestShape(ctx context.Context, shape *RequestShape) {
	if m := metaFrom(ctx); m != nil && shape != nil {
		m.shape = shape
	}
}

func requestShapeFrom(ctx context.Context) *RequestShape {
	if m := metaFrom(ctx); m != nil {
		return m.shape
	}
	return nil
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
