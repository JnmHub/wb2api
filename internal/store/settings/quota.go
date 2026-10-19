package settings

import (
	"fmt"
	"strings"
)

// WindowProbe: the model-quota window probe is on unless explicitly switched
// off (legacy default: the key being absent means enabled).
func (s *Store) WindowProbe() bool {
	v, ok := s.View()["window_probe"]
	if !ok || v == nil {
		return true
	}
	b, ok := v.(bool)
	return ok && b
}

// SetWindowProbe stores the switch.
func (s *Store) SetWindowProbe(on bool) error {
	return s.Update(func(d Doc) error { d["window_probe"] = on; return nil })
}

// QuotaScanModels is the list of models the active quota scan watches.
func (s *Store) QuotaScanModels() []string {
	return normaliseModelList(s.View()["quota_scan_models"])
}

// SetQuotaScanModels stores the watched list (at most 200).
func (s *Store) SetQuotaScanModels(list []string) ([]string, error) {
	if len(list) > 200 {
		return nil, fmt.Errorf("quota_scan_models: at most 200 models")
	}
	clean := normaliseModelList(stringsToAny(list))
	err := s.Update(func(d Doc) error {
		d["quota_scan_models"] = stringsToAny(clean)
		return nil
	})
	return clean, err
}

// normaliseModelList accepts the string form ("a, b; c") the legacy panel wrote
// as well as a JSON list, trims, drops empties and de-duplicates in order.
func normaliseModelList(v any) []string {
	var raw []string
	switch t := v.(type) {
	case string:
		raw = strings.FieldsFunc(t, func(r rune) bool {
			return r == ',' || r == ';' || r == '\n' || r == '\r' || r == ' ' || r == '\t'
		})
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok {
				raw = append(raw, s)
			}
		}
	case []string:
		raw = t
	}
	out := []string{}
	seen := map[string]bool{}
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
