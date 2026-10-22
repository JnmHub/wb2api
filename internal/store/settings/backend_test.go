package settings

import (
	"errors"
	"maps"
	"sync/atomic"
	"testing"
)

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

func TestBackendRefreshCannotOverwriteCommittedUpdate(t *testing.T) {
	for _, failLoad := range []bool{false, true} {
		t.Run(map[bool]string{false: "stale load", true: "load error"}[failLoad], func(t *testing.T) {
			started, resume := make(chan struct{}), make(chan struct{})
			var reads atomic.Int32
			s := OpenWithUpdater(
				func() (Doc, error) {
					if reads.Add(1) == 1 {
						close(started)
						<-resume
					}
					if failLoad {
						return nil, errors.New("load unavailable")
					}
					return Doc{"version": "old"}, nil
				}, nil,
				func(fn func(Doc) error) (Doc, error) {
					doc := Doc{"version": "old"}
					if err := fn(doc); err != nil {
						return nil, err
					}
					return doc, nil
				},
			)
			done := make(chan Doc, 1)
			go func() { done <- s.View() }()
			<-started
			if err := s.Update(func(d Doc) error { d["version"] = "committed"; return nil }); err != nil {
				t.Fatal(err)
			}
			close(resume)
			if got := <-done; got["version"] != "committed" {
				t.Fatalf("refresh returned stale state: %v", got)
			}
			if got := s.View(); got["version"] != "committed" {
				t.Fatalf("refresh overwrote committed cache: %v", got)
			}
		})
	}
}

func TestOpenWithAtomicUpdate(t *testing.T) {
	doc := Doc{"existing": true}
	updates := 0
	s := OpenWithUpdater(
		func() (Doc, error) { return maps.Clone(doc), nil },
		func(next Doc) error { doc = maps.Clone(next); return nil },
		func(fn func(Doc) error) (Doc, error) {
			updates++
			next := maps.Clone(doc)
			if err := fn(next); err != nil {
				return nil, err
			}
			doc = maps.Clone(next)
			return next, nil
		},
	)
	called := 0
	if err := s.Update(func(d Doc) error {
		called++
		d["added"] = "value"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if updates != 1 || called != 1 || doc["existing"] != true || doc["added"] != "value" {
		t.Fatalf("updates=%d callback=%d doc=%v", updates, called, doc)
	}
	if s.View()["added"] != "value" {
		t.Fatalf("atomic update not published: %v", s.View())
	}

	wantErr := errors.New("reject mutation")
	if err := s.Update(func(d Doc) error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("Update error = %v, want %v", err, wantErr)
	}
	if updates != 2 || called != 1 || doc["added"] != "value" {
		t.Fatalf("failed mutation changed backend: updates=%d callback=%d doc=%v", updates, called, doc)
	}
}
