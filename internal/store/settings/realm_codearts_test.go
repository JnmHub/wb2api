package settings

import "testing"

// A key may be bound to any known channel; an unknown value must be rejected at
// write time instead of silently falling back to the international exit.
func TestKeyInputRealmChannels(t *testing.T) {
	for _, ok := range []string{"", "intl", "cn", "codearts", " CodeArts "} {
		v := ok
		if err := (KeyInput{Realm: &v}).validate(); err != nil {
			t.Fatalf("realm %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"huawei", "codeart", "intl2", "all"} {
		v := bad
		if err := (KeyInput{Realm: &v}).validate(); err == nil {
			t.Fatalf("realm %q accepted, want a validation error", bad)
		}
	}
}
