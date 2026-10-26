package settings

import "strings"

// ModelAliases maps client-facing model names to the real upstream ids, so a
// client that only knows e.g. a claude-* name can be served by the models this
// gateway actually has. Matching is case-insensitive; a value may not chain into
// another alias (one hop only), which keeps the mapping predictable.
func (s *Store) ModelAliases() map[string]string {
	d := s.View()
	raw, ok := d["model_aliases"].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		name := strings.ToLower(strings.TrimSpace(k))
		target := strings.TrimSpace(truthyStr(v))
		if name == "" || target == "" || strings.EqualFold(name, target) {
			continue
		}
		out[name] = target
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ResolveModelAlias returns the upstream id for a client-facing name.
func (s *Store) ResolveModelAlias(name string) (string, bool) {
	aliases := s.ModelAliases()
	if len(aliases) == 0 {
		return "", false
	}
	real, ok := aliases[strings.ToLower(strings.TrimSpace(name))]
	return real, ok
}

// SetModelAliases replaces the whole map. An empty map clears it.
func (s *Store) SetModelAliases(in map[string]string) error {
	clean := map[string]any{}
	for k, v := range in {
		name := strings.ToLower(strings.TrimSpace(k))
		target := strings.TrimSpace(v)
		if name == "" || target == "" || strings.EqualFold(name, target) {
			continue
		}
		clean[name] = target
	}
	return s.Update(func(d Doc) error {
		if len(clean) == 0 {
			delete(d, "model_aliases")
			return nil
		}
		d["model_aliases"] = clean
		return nil
	})
}
