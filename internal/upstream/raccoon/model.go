package raccoon

import (
	"encoding/json"
	"strconv"
	"strings"

	"wb2api/internal/store/accounts"
)

// Model is one catalogue entry, normalised from the upstream's
// /model_catalog shape: data.categories[].models[].
type Model struct {
	// ID is the upstream model id (what the chat body must carry).
	ID string
	// Name is the display name.
	Name string
	// Description is the marketing line (display_description).
	Description string
	// ContextWindow and MaxTokens are 0 when the upstream did not say. They
	// are never invented: a fake limit is worse than an unknown one.
	ContextWindow int64
	MaxTokens     int64
	// SupportsImage/SupportsThinking are capability flags.
	SupportsImage    bool
	SupportsThinking bool
	// Visible mirrors the upstream's own "show it in the client" flag.
	Visible bool
	// Multiplier is the credit multiplier (billing_effective_multiplier, with
	// billing_multiplier as fallback). 0 = unknown.
	Multiplier float64
	// Levels is the thinking-effort set the upstream advertises for this model
	// (params.raccoon_thinking.display_levels). Empty = unknown.
	Levels []string
	// Kind is "chat" or "image".
	Kind string
}

// IsImageModel reports the synthetic image entry, which the gateway must not
// advertise while it has no /v1/images surface.
func IsImageModel(id string) bool { return strings.EqualFold(strings.TrimSpace(id), ImageModelID) }

// ModelsOf returns the model ids discovered for this account. An empty result
// means "not discovered yet": callers must treat eligibility as unknown rather
// than "can serve nothing".
func ModelsOf(models []Model) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		if m.ID == "" || IsImageModel(m.ID) {
			continue
		}
		out = append(out, m.ID)
	}
	return out
}

// SupportsModel reports whether a discovered catalogue contains the model.
// An empty catalogue (not yet discovered) allows everything.
func SupportsModel(models []Model, model string) bool {
	if len(models) == 0 || model == "" {
		return true
	}
	for _, m := range models {
		if strings.EqualFold(m.ID, model) {
			return true
		}
	}
	return false
}

// ModelsOfRecord returns the model ids discovered for this account, read from
// the raccoon block of the account file. An empty result means "not discovered
// yet": callers must treat eligibility as unknown, not as "serves nothing".
func ModelsOfRecord(rec *accounts.Record) []string {
	if rec == nil {
		return nil
	}
	fields := rec.RaccoonFields()
	raw, ok := fields["models"]
	if !ok {
		return nil
	}
	return stringList(raw)
}

// VisibleModelsOfRecord returns the advertisable subset stored by discovery.
// When an older record has no such subset, the caller falls back to the full
// catalogue.
func VisibleModelsOfRecord(rec *accounts.Record) []string {
	if rec == nil {
		return nil
	}
	fields := rec.RaccoonFields()
	raw, ok := fields["models_visible"]
	if !ok {
		return nil
	}
	return stringList(raw)
}

// CatalogueDiscovered distinguishes a successful empty catalogue from an account
// that has not discovered its models yet.
func CatalogueDiscovered(rec *accounts.Record) bool {
	if len(ModelsOfRecord(rec)) > 0 || VisibleModelsOfRecord(rec) != nil {
		return true
	}
	if rec == nil {
		return false
	}
	fields := rec.RaccoonFields()
	_, hasMetadata := fields["models_meta"].([]any)
	return hasMetadata || (ModelsDiscoveredAt(rec) > 0 && accounts.RaccoonStringOf(fields, "models_error") == "")
}

// PublicModelsOfRecord returns the chat ids this account can advertise.
func PublicModelsOfRecord(rec *accounts.Record) []string {
	if visible := VisibleModelsOfRecord(rec); visible != nil {
		return visible
	}
	if meta := MetaOfRecord(rec); meta != nil {
		var visible []string
		for _, m := range meta {
			if m.Kind == "chat" && m.Visible && !IsImageModel(m.ID) {
				visible = append(visible, m.ID)
			}
		}
		return visible
	}
	// Older records stored the whole catalogue. Internal Raccoon-Work ids
	// from that format stay callable by name without being advertised.
	models := ModelsOfRecord(rec)
	visible := models[:0]
	for _, id := range models {
		if !strings.HasPrefix(strings.ToLower(id), "raccoon-") {
			visible = append(visible, id)
		}
	}
	return visible
}

// ModelsDiscoveredAt returns models_at as a unix second (0 = never).
func ModelsDiscoveredAt(rec *accounts.Record) int64 {
	if rec == nil {
		return 0
	}
	switch v := rec.RaccoonFields()["models_at"].(type) {
	case json.Number:
		at, _ := v.Int64()
		return at
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case string:
		at, _ := strconv.ParseInt(v, 10, 64)
		return at
	}
	return 0
}

func stringList(raw any) []string {
	switch list := raw.(type) {
	case []string:
		out := make([]string, 0, len(list))
		for _, s := range list {
			if t := strings.TrimSpace(s); t != "" && !IsImageModel(t) {
				out = append(out, t)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" && !IsImageModel(s) {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	}
	return nil
}

// ReasoningLevels lists the effort values the channel honours for a model, so
// the panel can render the same chip set the upstream documents.
func ReasoningLevels(model string) []string {
	switch family(model) {
	case "thinking":
		return []string{"low", "medium", "high", "max"}
	case "low_high":
		return []string{"low", "high"}
	case "effort":
		return []string{"low", "medium", "high"}
	}
	return nil
}

// MetaOfRecord returns the per-model metadata stored by discovery. Records
// written before models_meta existed return nil; callers then fall back to the
// shipped snapshot.
func MetaOfRecord(rec *accounts.Record) []Model {
	if rec == nil {
		return nil
	}
	raw, ok := rec.RaccoonFields()["models_meta"].([]any)
	if !ok {
		return nil
	}
	out := make([]Model, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		model := Model{
			ID:               str(m, "id"),
			Name:             str(m, "name"),
			Description:      str(m, "description"),
			ContextWindow:    intOf(m, nil, "context_window"),
			MaxTokens:        intOf(m, nil, "max_tokens"),
			SupportsImage:    boolValue(m["supports_images"]),
			SupportsThinking: boolValue(m["supports_reasoning"]),
			Visible:          boolValue(m["visible"]),
			Multiplier:       floatOf(m, "multiplier"),
			Kind:             firstNonEmpty(str(m, "kind"), "chat"),
			Levels:           stringList(m["reasoning_levels"]),
		}
		if model.ID == "" {
			continue
		}
		out = append(out, model)
	}
	return out
}

// EncodeMeta turns models into the JSON shape stored on the account.
func EncodeMeta(models []Model) []any {
	out := make([]any, 0, len(models))
	for _, m := range models {
		out = append(out, map[string]any{
			"id": m.ID, "name": m.Name, "description": m.Description,
			"context_window": m.ContextWindow, "max_tokens": m.MaxTokens,
			"supports_images": m.SupportsImage, "supports_reasoning": m.SupportsThinking,
			"visible": m.Visible, "multiplier": m.Multiplier, "kind": m.Kind,
			"reasoning_levels": m.Levels,
		})
	}
	return out
}

func boolValue(v any) bool {
	b, _ := v.(bool)
	return b
}

// KnownModel reports whether the shipped catalogue lists a chat model.
func KnownModel(id string) bool {
	want := WireName(id)
	for _, m := range fallbackModels() {
		if m.Kind == "chat" && strings.EqualFold(m.ID, want) {
			return true
		}
	}
	return false
}

// AllowsModel reports whether this account can be sent the requested model.
func AllowsModel(rec *accounts.Record, model string) bool {
	_, ok := ResolveChatModel(rec, model)
	return ok
}

// ResolveChatModel prefers the account's discovered catalogue and falls back to
// verified chat ids before discovery. Unknown ids can silently select the
// upstream's default model, so they cannot be sent speculatively.
func ResolveChatModel(rec *accounts.Record, model string) (string, bool) {
	want := WireName(model)
	if want == "" {
		return "", false
	}
	models := ModelsOfRecord(rec)
	if len(models) == 0 && !CatalogueDiscovered(rec) {
		return want, KnownModel(want)
	}
	for _, id := range models {
		if strings.EqualFold(id, want) {
			return id, true
		}
	}
	return "", false
}

// WireName maps a client-visible model name onto a canonical id: it trims the
// path spelling some clients add ("raccoon/sn-glm-5-3" -> "sn-glm-5-3") and
// resolves a vendor-neutral alias ("deepseek-v4.1-flash" ->
// "sn-deepseek-v4-1-flash-1017"). ResolveChatModel recovers the vendor's spelling.
func WireName(model string) string {
	m := strings.TrimSpace(model)
	if i := strings.LastIndexByte(m, '/'); i >= 0 && i < len(m)-1 {
		m = m[i+1:]
	}
	if id := Alias(m); id != "" {
		return id
	}
	return strings.ToLower(m)
}
