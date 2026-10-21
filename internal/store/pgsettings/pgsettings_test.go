package pgsettings

import "testing"

func TestDecodeSettings(t *testing.T) {
	doc, err := decode([]byte(`{"upstream":{"header_timeout_seconds":30},"n":1}`))
	if err != nil {
		t.Fatal(err)
	}
	up, _ := doc["upstream"].(map[string]any)
	if up["header_timeout_seconds"] == nil || doc["n"] == nil {
		t.Fatalf("doc = %v", doc)
	}
}
