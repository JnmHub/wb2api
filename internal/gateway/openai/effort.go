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

// effortLevels is the vocabulary the upstream accepts for reasoning_effort.
// Anything else is rejected (11150), so unknown client spellings are folded into
// the nearest accepted level instead of being forwarded as-is.
var effortAliases = map[string]string{
	"none": "none", "off": "none", "disabled": "none", "false": "none", "0": "none",
	"min": "minimal", "minimum": "minimal", "minimal": "minimal", "lowest": "minimal",
	"low": "low",
	"med": "medium", "medium": "medium", "default": "medium", "auto": "medium",
	"high": "high", "hi": "high", "max": "high", "maximum": "high", "highest": "high",
	"xhigh": "high", "ultra": "high", "extreme": "high",
}

// normalizeEffort folds a client spelling into an accepted level. Values
// already in the accepted set are returned unchanged; an unknown value is
// dropped ("" means "let the model decide") rather than sent and rejected.
func normalizeEffort(v string) string {
	name := strings.ToLower(strings.TrimSpace(v))
	if name == "" {
		return ""
	}
	if lvl, ok := effortAliases[name]; ok {
		return lvl
	}
	return ""
}

// applyEffortMap rewrites the outgoing reasoning effort: a model the catalogue
// pins to one level always sends that level, and a client spelling the upstream
// does not know is normalised. The client payload itself is untouched, so the
// streamed/inspected request keeps what the caller asked for.
func (h *Handler) applyEffortMap(q *request) {
	body := q.shaped
	if body == nil || q.model == "" {
		return
	}
	if cat, ok := h.Catalog.(effortCatalog); ok {
		if fixed := cat.FixedEffort(q.model); fixed != "" {
			body.Set("reasoning_effort", fixed)
			deleteKey(body, "reasoningEffort")
			return
		}
	}
	given := clientEffort(body)
	if given == "" {
		return
	}
	if lvl := normalizeEffort(given); lvl != given {
		deleteKey(body, "reasoningEffort")
		if lvl == "" {
			deleteKey(body, "reasoning_effort")
			return
		}
		body.Set("reasoning_effort", lvl)
	}
}

func deleteKey(o *jsonx.Obj, key string) {
	if o != nil {
		o.Delete(key)
	}
}
