package openai

import (
	"strings"

	"wb2api/internal/jsonx"
)

// effortCatalog is the optional part of the catalogue that knows which models
// are pinned to one reasoning level.
type effortCatalog interface {
	FixedEffort(model string) string
	DefaultEffort(model string) string
}

func clientEffort(body *jsonx.Obj) string {
	if body == nil {
		return ""
	}
	for _, k := range []string{"reasoning_effort", "reasoningEffort"} {
		if v := body.Get(k); jsonx.Truthy(v) {
			return jsonx.PyStr(v)
		}
	}
	return ""
}

// effort is the reasoning effort the request actually runs at ("" = unknown),
// resolved the way the upstream applies it: a model the catalogue pins to one
// level always runs there; a request that switched thinking off or asked for
// "none" ran without reasoning; otherwise the client's own value wins, and
// otherwise the model's declared default applies.
func (q *request) effort(h *Handler) string {
	if q.effortDone {
		return q.effortVal
	}
	q.effortDone = true
	body := q.shaped
	model := q.model
	cat, _ := h.Catalog.(effortCatalog)
	if model != "" && cat != nil {
		if fixed := cat.FixedEffort(model); fixed != "" {
			q.effortVal = fixed
			return fixed
		}
	}
	if body != nil {
		if th, ok := body.Get("thinking").(*jsonx.Obj); ok {
			if strings.ToLower(strings.TrimSpace(jsonx.PyStrIfTruthy(th.Get("type")))) == "disabled" {
				q.effortVal = "none"
				return "none"
			}
		}
	}
	given := clientEffort(body)
	switch {
	case strings.ToLower(strings.TrimSpace(given)) == "none":
		q.effortVal = "none"
	case given != "":
		q.effortVal = given
	case model != "" && cat != nil:
		q.effortVal = cat.DefaultEffort(model)
	}
	return q.effortVal
}
