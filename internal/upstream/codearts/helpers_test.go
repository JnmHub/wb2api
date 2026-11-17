package codearts

import (
	"encoding/base64"
	"testing"

	"wb2api/internal/jsonx"
)

// test helpers shared by the package tests.

func newObj(t *testing.T, raw string) *jsonx.Obj {
	t.Helper()
	o, err := jsonx.UnmarshalObj([]byte(raw))
	if err != nil {
		t.Fatalf("bad fixture JSON: %v", err)
	}
	return o
}

func parseObj(t *testing.T, raw []byte) *jsonx.Obj {
	t.Helper()
	o, err := jsonx.UnmarshalObj(raw)
	if err != nil {
		t.Fatalf("response is not a JSON object: %v (%s)", err, raw)
	}
	return o
}

func decodeSegment(t *testing.T, seg string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("segment is not base64url: %v", err)
	}
	return string(raw)
}
