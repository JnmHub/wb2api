package settings

import "strings"

// Model deny-list. Port of legacy is_model_banned: an explicit allowlist wins,
// then an allowlist prefix, then a blocked substring, then the exact blocked
// set. All four are empty by default, so a fresh install bans nothing.

func lowerList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strings.ToLower(s))
	}
	return out
}

func (s *Store) BannedModels() []string {
	return lowerList(normaliseModelList(s.View()["banned_models"]))
}

func (s *Store) AllowedModels() []string {
	return lowerList(normaliseModelList(s.View()["allowed_models"]))
}

func (s *Store) AllowedPrefixes() []string {
	return lowerList(normaliseModelList(s.View()["allowed_prefixes"]))
}

func (s *Store) BannedSubstring() string {
	v, _ := s.View()["banned_substring"].(string)
	return strings.ToLower(strings.TrimSpace(v))
}

// SetBannedModels stores the exact deny-list (lower-cased, de-duplicated).
func (s *Store) SetBannedModels(list []string) ([]string, error) {
	clean := lowerList(normaliseModelList(stringsToAny(list)))
	err := s.Update(func(d Doc) error {
		d["banned_models"] = stringsToAny(clean)
		return nil
	})
	return clean, err
}

// SetBannedSubstring stores the substring rule ("" clears it).
func (s *Store) SetBannedSubstring(sub string) error {
	return s.Update(func(d Doc) error {
		d["banned_substring"] = strings.ToLower(strings.TrimSpace(sub))
		return nil
	})
}

// ModelBanned reports whether a model must be refused locally.
func (s *Store) ModelBanned(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return false
	}
	for _, a := range s.AllowedModels() {
		if m == a {
			return false
		}
	}
	for _, p := range s.AllowedPrefixes() {
		if p != "" && strings.HasPrefix(m, p) {
			return false
		}
	}
	if sub := s.BannedSubstring(); sub != "" && strings.Contains(m, sub) {
		return true
	}
	for _, b := range s.BannedModels() {
		if m == b {
			return true
		}
	}
	return false
}
