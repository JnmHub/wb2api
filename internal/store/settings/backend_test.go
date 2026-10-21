package settings

import "testing"

func TestOpenWithBackend(t *testing.T) {
	doc := Doc{"upstream": map[string]any{"header_timeout_seconds": 30}}
	rev := 0
	s := OpenWith(
		func() (Doc, error) { return doc, nil },
		func(next Doc) error { doc = next; rev++; return nil },
	)
	if got := s.UpstreamConfig().HeaderTimeout; got.Seconds() != 30 {
		t.Fatalf("header timeout = %v", got)
	}
	if err := s.Update(func(d Doc) error {
		d["panel_language"] = "zh"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rev != 1 || doc["panel_language"] != "zh" {
		t.Fatalf("backend save missing: rev=%d doc=%v", rev, doc)
	}
	// A save must be visible immediately without another backend read.
	if got := s.View()["panel_language"]; got != "zh" {
		t.Fatalf("saved value not visible: %v", got)
	}
}
