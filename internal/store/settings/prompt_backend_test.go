package settings

import (
	"maps"
	"testing"
	"time"
)

func TestSetPromptPreservesTransactionalSibling(t *testing.T) {
	for _, tc := range []struct {
		name   string
		latest map[string]any
		patch  map[string]any
		want   PromptConfig
	}{
		{
			name:   "mode preserves file",
			latest: map[string]any{"mode": "passthrough", "file": "latest.txt", "future_option": true},
			patch:  map[string]any{"mode": " APPEND "},
			want:   PromptConfig{Mode: "append", File: "latest.txt"},
		},
		{
			name:   "file preserves mode",
			latest: map[string]any{"mode": "custom", "file": "old.txt", "future_option": true},
			patch:  map[string]any{"file": " new.txt "},
			want:   PromptConfig{Mode: "custom", File: "new.txt"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := Doc{"prompt": maps.Clone(tc.latest), "other": "preserved"}
			updates := 0
			s := OpenWithUpdater(
				func() (Doc, error) {
					loaded := maps.Clone(stored)
					loaded["prompt"] = maps.Clone(stored["prompt"].(map[string]any))
					return loaded, nil
				}, nil,
				func(fn func(Doc) error) (Doc, error) {
					updates++
					next := maps.Clone(stored)
					next["prompt"] = maps.Clone(stored["prompt"].(map[string]any))
					if err := fn(next); err != nil {
						return nil, err
					}
					stored = next
					return next, nil
				},
			)
			// Another replica has already committed stored, but this replica's
			// cache still contains the older sibling values. Keep it stale without
			// sleeps or racing the polling interval.
			s.snap.Store(&snapshot{exists: true, doc: Doc{
				"prompt": map[string]any{"mode": "passthrough", "file": "old.txt"},
			}})
			s.lastCheck.Store(time.Now().Add(time.Hour).UnixNano())
			got, err := s.SetPrompt(tc.patch)
			if err != nil || got != tc.want || updates != 1 {
				t.Fatalf("SetPrompt = %+v, %v; updates=%d; want %+v", got, err, updates, tc.want)
			}
			prompt := stored["prompt"].(map[string]any)
			if prompt["mode"] != tc.want.Mode || prompt["file"] != tc.want.File || prompt["future_option"] != true || stored["other"] != "preserved" {
				t.Fatalf("partial prompt update lost stored fields: %v", stored)
			}
		})
	}
}
