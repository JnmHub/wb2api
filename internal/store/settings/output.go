package settings

import "fmt"

// OutputTPSLimit is the global default streaming token-rate cap (0 = off).
func (s *Store) OutputTPSLimit() float64 {
	f := floatOf(s.View()["output_tps_limit"])
	if f < 0 {
		return 0
	}
	if f > 1000 {
		return 1000
	}
	return f
}

// SetOutputTPSLimit stores the global default. 0 disables pacing.
func (s *Store) SetOutputTPSLimit(tps float64) error {
	if tps < 0 || tps > 1000 {
		return fmt.Errorf("output_tps_limit must be between 0 and 1000")
	}
	if tps > 0 && tps < 0.1 {
		return fmt.Errorf("output_tps_limit must be at least 0.1 when enabled")
	}
	return s.Update(func(d Doc) error { d["output_tps_limit"] = tps; return nil })
}

// FilterModelsByKey reports whether /v1/models should hide models a key cannot use.
func (s *Store) FilterModelsByKey() bool {
	b, _ := s.View()["filter_models_by_key"].(bool)
	return b
}
