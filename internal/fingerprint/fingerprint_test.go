package fingerprint

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

// TestGoldenVectors checks DeriveID against values produced by the legacy
// Python implementation (tools/genvectors/gen.py).
func TestGoldenVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs []struct{ UID, Salt, ID string }
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	if len(vs) == 0 {
		t.Fatal("no vectors")
	}
	for _, v := range vs {
		if got := DeriveID(v.UID, v.Salt); got != v.ID {
			t.Errorf("DeriveID(%q,%q) = %s, want %s", v.UID, v.Salt, got, v.ID)
		}
	}
}

func TestShapes(t *testing.T) {
	uuidRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-a[0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidRe.MatchString(MachineID("u1")) || !uuidRe.MatchString(SessionID("u1")) {
		t.Fatal("ids are not UUIDv4-shaped")
	}
	if MachineID("u1") == SessionID("u1") {
		t.Fatal("machine and session ids must differ")
	}
	if MachineID("u1") == MachineID("u2") {
		t.Fatal("different accounts must get different ids")
	}
	hexRe := regexp.MustCompile(`^[0-9a-f]{32}$`)
	a, b := NewRequestID(), NewRequestID()
	if !hexRe.MatchString(a) || a == b {
		t.Fatalf("bad request ids %q %q", a, b)
	}
}
