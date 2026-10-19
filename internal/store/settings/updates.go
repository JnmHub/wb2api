package settings

// Update discovery state. The switch is off by default: a fresh gateway makes
// no outbound request at all until the operator turns it on.

// UpdateCheckEnabled reports the opt-in switch.
func (s *Store) UpdateCheckEnabled() bool {
	b, _ := s.View()["update_check_enabled"].(bool)
	return b
}

// SetUpdateCheckEnabled stores the switch.
func (s *Store) SetUpdateCheckEnabled(on bool) error {
	return s.Update(func(d Doc) error { d["update_check_enabled"] = on; return nil })
}

// UpdateCheckState returns the small persisted record:
// last_attempt / last_success / latest_version. No URL or response body.
func (s *Store) UpdateCheckState() map[string]any {
	raw, _ := s.View()["update_check"].(map[string]any)
	out := map[string]any{"last_attempt": "", "last_success": false, "latest_version": ""}
	for k, v := range raw {
		switch k {
		case "last_attempt", "latest_version":
			if str, ok := v.(string); ok {
				out[k] = str
			}
		case "last_success":
			if b, ok := v.(bool); ok {
				out[k] = b
			}
		}
	}
	return out
}

// RecordUpdateCheck persists the outcome of one attempt.
func (s *Store) RecordUpdateCheck(lastAttempt string, success bool, latest string) error {
	return s.Update(func(d Doc) error {
		d["update_check"] = map[string]any{
			"last_attempt": lastAttempt, "last_success": success, "latest_version": latest,
		}
		return nil
	})
}
