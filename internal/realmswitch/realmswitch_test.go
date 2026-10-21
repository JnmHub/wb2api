package realmswitch

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir, "intl")
	if got := s.Get(); got != "intl" {
		t.Fatalf("default realm = %q, want intl", got)
	}
	if s.Set("cn") != true {
		t.Fatal("Set(cn) failed")
	}
	if got := Open(dir, "intl").Get(); got != "cn" {
		t.Fatalf("reopened realm = %q, want cn", got)
	}
	if s.Set("mars") {
		t.Fatal("Set(mars) must be refused")
	}
	if got := s.Get(); got != "cn" {
		t.Fatalf("invalid Set changed realm to %q", got)
	}
	if filepath.Base(s.path) != "active_realm.json" {
		t.Fatalf("unexpected path %q", s.path)
	}
}

func TestBackendRefreshesAcrossReplicas(t *testing.T) {
	var mu sync.Mutex
	shared := "intl"
	load := func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		return shared, nil
	}
	save := func(realm string) error {
		mu.Lock()
		defer mu.Unlock()
		shared = realm
		return nil
	}
	one := OpenWith(load, save, "intl")
	two := OpenWith(load, save, "intl")
	two.interval = time.Nanosecond

	if !one.Set("cn") {
		t.Fatal("Set(cn) failed")
	}
	if got := one.Get(); got != "cn" {
		t.Fatalf("writer realm = %q, want cn", got)
	}
	time.Sleep(time.Millisecond)
	if got := two.Get(); got != "cn" {
		t.Fatalf("reader realm = %q, want cn after refresh", got)
	}
}

func TestBackendSaveFailureKeepsCurrent(t *testing.T) {
	s := OpenWith(
		func() (string, error) { return "intl", nil },
		func(string) error { return errors.New("database down") },
		"intl",
	)
	if s.Set("cn") {
		t.Fatal("Set must report the save failure")
	}
	if got := s.Get(); got != "intl" {
		t.Fatalf("realm = %q after failed save, want intl", got)
	}
}
