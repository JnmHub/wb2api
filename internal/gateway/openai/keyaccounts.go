package openai

import "wb2api/internal/store/settings"

// accountAllowed reports whether a UID matches one of the key's account
// patterns. An empty pattern list allows every account.
func accountAllowed(patterns []string, uid string) bool {
	if len(patterns) == 0 {
		return true
	}
	name := uid
	for _, p := range patterns {
		if settings.FnMatchCase(lowerASCII(name), p) {
			return true
		}
	}
	return false
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// accountsDeniedFor turns a key's allow-list into the exclusion set the pool
// picker expects, so the whole existing policy (sticky, weight, TopK, page)
// keeps working unchanged and never sees a forbidden account.
func (h *Handler) accountsDeniedFor(patterns []string) map[string]struct{} {
	if len(patterns) == 0 || h.Pool == nil {
		return nil
	}
	denied := map[string]struct{}{}
	for _, a := range h.Pool.All() {
		if a == nil {
			continue
		}
		if !accountAllowed(patterns, a.UID) {
			denied[a.UID] = struct{}{}
		}
	}
	if len(denied) == 0 {
		return nil
	}
	return denied
}
