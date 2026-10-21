package settings

import "testing"

func TestOutputTPSLimitDefaultsAndValidation(t *testing.T) {
	s := Open(t.TempDir())
	if got := s.OutputTPSLimit(); got != 0 {
		t.Fatalf("default = %v", got)
	}
	if err := s.SetOutputTPSLimit(20); err != nil || s.OutputTPSLimit() != 20 {
		t.Fatalf("set 20: %v %v", err, s.OutputTPSLimit())
	}
	if err := s.SetOutputTPSLimit(0); err != nil || s.OutputTPSLimit() != 0 {
		t.Fatalf("set 0: %v %v", err, s.OutputTPSLimit())
	}
	if err := s.SetOutputTPSLimit(-1); err == nil {
		t.Fatal("negative limit must be rejected")
	}
	if err := s.SetOutputTPSLimit(0.05); err == nil {
		t.Fatal("sub-0.1 limit must be rejected")
	}
}

func TestKeyTPSLimit(t *testing.T) {
	s := Open(t.TempDir())
	tps := 12.5
	k, err := s.CreateKey(KeyInput{Name: ptr("tps"), TPSLimit: &tps})
	if err != nil {
		t.Fatal(err)
	}
	got := s.APIKeys(false)
	if len(got) != 1 || got[0].ID != k.ID || got[0].TPSLimit != 12.5 {
		t.Fatalf("keys = %+v", got)
	}
	off := -1.0
	if err := s.UpdateKey(k.ID, KeyInput{TPSLimit: &off}); err != nil {
		t.Fatal(err)
	}
	if got := s.APIKeys(false)[0].TPSLimit; got != -1 {
		t.Fatalf("tps = %v", got)
	}
}
