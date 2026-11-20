package raccoon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"wb2api/internal/upstream/identity"
)

// ErrEmptyCatalog marks a successful HTTP call that carried no usable model.
// Callers keep the previous list instead of blanking the account.
var ErrEmptyCatalog = errors.New("小浣熊模型目录为空")

// FetchCatalog pulls /model_catalog for one credential. The endpoint needs a
// token and the organisation code (a personal account sends none).
func (c *Client) FetchCatalog(ctx context.Context, cred Credential) ([]Model, error) {
	headers := identity.Headers{{Name: "Accept", Value: "application/json"}}
	if token := StripBearer(cred.Token); token != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	if org := IAMOrg(cred); org != "" {
		headers.Set("X-Org-Code", org)
	}
	resp, err := c.Do(ctx, http.MethodGet, c.llmBase()+ModelCatalogPath, headers, nil)
	if err != nil {
		return nil, fmt.Errorf("小浣熊模型目录拉取失败: %w", err)
	}
	if resp.TokenExpired() {
		return nil, ErrNeedsLogin
	}
	if !resp.OK() {
		return nil, fmt.Errorf("小浣熊模型目录拉取失败（HTTP %d）: %s", resp.Status, resp.Message)
	}
	models := ParseCatalog(resp.Data)
	if len(models) == 0 {
		return nil, ErrEmptyCatalog
	}
	return models, nil
}

// ParseCatalog normalises the catalogue envelope. Only the "chat" category is
// kept (a missing type counts as chat), duplicates resolve to the first entry,
// and an entry without an id is dropped.
func ParseCatalog(data map[string]any) []Model {
	if data == nil {
		return nil
	}
	// Some responses nest the payload one level deeper; accept both.
	if _, ok := data["categories"]; !ok {
		if inner, ok := data["data"].(map[string]any); ok {
			data = inner
		}
	}
	categories, _ := data["categories"].([]any)
	seen := map[string]bool{}
	out := make([]Model, 0, len(categories)*4)
	for _, raw := range categories {
		cat, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := cat["type"].(string); kind != "" && !strings.EqualFold(kind, "chat") {
			continue
		}
		list, _ := cat["models"].([]any)
		for _, item := range list {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			model, ok := normaliseModel(m)
			if !ok || seen[strings.ToLower(model.ID)] {
				continue
			}
			seen[strings.ToLower(model.ID)] = true
			out = append(out, model)
		}
	}
	return out
}

func normaliseModel(m map[string]any) (Model, bool) {
	id := firstNonEmpty(str(m, "name"), str(m, "model_name"), str(m, "id"))
	if id == "" {
		return Model{}, false
	}
	out := Model{ID: id, Visible: true, Kind: "chat"}
	label := str(m, "display_name")
	if label == "" {
		// description is the internal codename on this upstream; use it as a
		// label only when it is short enough to be one.
		if d := str(m, "description"); d != "" && len([]rune(d)) <= 64 {
			label = d
		}
	}
	if label == "" {
		label = id
	}
	out.Name = label
	out.Description = str(m, "display_description")

	params, _ := m["params"].(map[string]any)
	out.ContextWindow = intOf(m, params, "context_window", "contextWindow")
	out.MaxTokens = intOf(m, params, "max_tokens", "maxTokens")
	// billing_effective_multiplier is the price actually charged and may be 0
	// (a discounted/free model). Presence, not truthiness, decides which field
	// wins: falling back on 0 would report a free model at its list price.
	if v, ok := floatField(m, "billing_effective_multiplier"); ok {
		out.Multiplier = v
	} else if v, ok := floatField(m, "billing_multiplier"); ok {
		out.Multiplier = v
	}

	if v, ok := boolField(m, "supports_images", "supportImage"); ok {
		out.SupportsImage = v
	} else {
		out.SupportsImage = tagContains(m, "vision", "image", "image-understanding")
	}
	// The upstream describes thinking through params.raccoon_thinking rather
	// than a boolean flag; the labels double as the effort set the model honours.
	if thinking, ok := params["raccoon_thinking"].(map[string]any); ok {
		if supported, ok := thinking["supported"].(bool); ok {
			out.SupportsThinking = supported
		}
		if levels, ok := thinking["display_levels"].([]any); ok {
			for _, raw := range levels {
				item, _ := raw.(map[string]any)
				if label := str(item, "label"); label != "" {
					out.Levels = append(out.Levels, strings.ToLower(label))
				}
			}
		}
		if out.SupportsThinking && len(out.Levels) == 0 {
			out.Levels = ReasoningLevels(id)
		}
	} else if v, ok := boolField(m, "supports_reasoning", "supportThinking"); ok {
		out.SupportsThinking = v
	} else {
		out.SupportsThinking = SupportsThinking(id)
	}
	if out.SupportsThinking && len(out.Levels) == 0 {
		out.Levels = ReasoningLevels(id)
	}
	if v, ok := m["visible"].(bool); ok {
		out.Visible = v
	}
	if strings.HasPrefix(strings.ToLower(id), "raccoon-image") || strings.HasSuffix(strings.ToLower(id), "-image") {
		out.Kind = "image"
	}
	return out, true
}

func str(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func intOf(m, params map[string]any, keys ...string) int64 {
	for _, k := range keys {
		for _, src := range []map[string]any{m, params} {
			if src == nil {
				continue
			}
			switch v := src[k].(type) {
			case float64:
				return int64(v)
			case int64:
				return v
			case int:
				return int64(v)
			case json.Number:
				if n, err := v.Int64(); err == nil {
					return n
				}
			}
		}
	}
	return 0
}

func floatOf(m map[string]any, key string) float64 {
	v, _ := floatField(m, key)
	return v
}

// floatField reports the value and whether the key was present and numeric.
func floatField(m map[string]any, key string) (float64, bool) {
	switch v := m[key].(type) {
	case float64:
		return v, true
	case int64:
		return float64(v), true
	case int:
		return float64(v), true
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return f, true
		}
	}
	return 0, false
}

func boolField(m map[string]any, keys ...string) (bool, bool) {
	for _, k := range keys {
		if v, ok := m[k].(bool); ok {
			return v, true
		}
	}
	return false, false
}

func tagContains(m map[string]any, needles ...string) bool {
	tags, _ := m["tags"].([]any)
	for _, raw := range tags {
		s, _ := raw.(string)
		s = strings.ToLower(strings.TrimSpace(s))
		for _, n := range needles {
			if s == n {
				return true
			}
		}
	}
	return false
}
