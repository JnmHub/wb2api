package settings

import "testing"

func TestOpsTuningDefaultsAndValidation(t *testing.T) {
	s := Open(t.TempDir())
	if got := s.OpsConcurrency(); got != 4 {
		t.Fatalf("default concurrency = %d", got)
	}
	if got := s.OpsIntervalMS(); got != 0 {
		t.Fatalf("default interval = %d", got)
	}
	if err := s.SetOpsConcurrency(2); err != nil || s.OpsConcurrency() != 2 {
		t.Fatalf("set concurrency: %v %d", err, s.OpsConcurrency())
	}
	if err := s.SetOpsIntervalMS(100); err != nil || s.OpsIntervalMS() != 100 {
		t.Fatalf("set interval: %v %d", err, s.OpsIntervalMS())
	}
	if err := s.SetOpsConcurrency(0); err == nil {
		t.Fatal("zero concurrency must be rejected")
	}
	if err := s.SetOpsConcurrency(17); err == nil {
		t.Fatal("17 concurrency must be rejected")
	}
	if err := s.SetOpsIntervalMS(-1); err == nil {
		t.Fatal("negative interval must be rejected")
	}
	if err := s.SetOpsIntervalMS(60001); err == nil {
		t.Fatal("60001 ms must be rejected")
	}
}
