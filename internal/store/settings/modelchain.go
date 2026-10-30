package settings

import "strings"

// MaxModelChain bounds one fallback list. Long chains mostly add upstream
// attempts (and cost) without changing the outcome, so the editor and the
// store both refuse more.
const MaxModelChain = 8

// ModelFallbacks maps a real upstream model id to the ordered models to try
// when that model cannot be served by any account any more (per-model 429
// window, upstream quota clock, missing entitlement).
//
// Values are flat: a fallback target is never expanded through its own entry,
// so a chain cannot loop and "flash → bunny → flash" stays exactly two steps.
func (s *Store) ModelFallbacks() map[string][]string {
	d := s.View()
	raw, ok := d["model_fallbacks"].(map[string]any)
	if !ok {
		return nil
	}
	out := map[string][]string{}
	for k, v := range raw {
		name := strings.ToLower(strings.TrimSpace(k))
		if name == "" {
			continue
		}
		chain := chainValues(v)
		if len(chain) == 0 {
			continue
		}
		out[name] = chain
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func chainValues(v any) []string {
	var items []any
	switch t := v.(type) {
	case []any:
		items = t
	case []string:
		for _, s := range t {
			items = append(items, s)
		}
	default:
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, item := range items {
		name := strings.ToLower(strings.TrimSpace(truthyStr(item)))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
		if len(out) >= MaxModelChain {
			break
		}
	}
	return out
}

// ModelChain returns what a request for model should try, in order: the model
// itself followed by its configured fallbacks. An empty result means the model
// has no chain and keeps the plain single-model behaviour.
func (s *Store) ModelChain(model string) []string {
	head := strings.ToLower(strings.TrimSpace(model))
	if head == "" {
		return nil
	}
	fallbacks := s.ModelFallbacks()[head]
	if len(fallbacks) == 0 {
		return []string{head}
	}
	out := []string{head}
	for _, name := range fallbacks {
		if name == head {
			continue
		}
		out = append(out, name)
	}
	return out
}

// SetModelFallbacks replaces the whole map. Entries that are empty, equal to
// their head, or longer than MaxModelChain are normalised away.
func (s *Store) SetModelFallbacks(in map[string][]string) error {
	clean := map[string]any{}
	for k, v := range in {
		head := strings.ToLower(strings.TrimSpace(k))
		if head == "" {
			continue
		}
		chain := chainValues(v)
		out := make([]any, 0, len(chain))
		for _, name := range chain {
			if name == head {
				continue
			}
			out = append(out, name)
		}
		if len(out) == 0 {
			continue
		}
		clean[head] = out
	}
	return s.Update(func(d Doc) error {
		if len(clean) == 0 {
			delete(d, "model_fallbacks")
			return nil
		}
		d["model_fallbacks"] = clean
		return nil
	})
}
