package settings

import "testing"

func TestDailyChatWebDefaultsAndBounds(t *testing.T) {
	s := Open(t.TempDir())
	if !s.DailyChatWeb() || !s.DailyChatWebRetry() {
		t.Fatal("the web leg and its retry default to on")
	}
	if got := s.DailyChatWebAttempts(); got != 3 {
		t.Fatalf("attempts = %d", got)
	}
	if got := s.DailyChatWebTimeout(); got != 120 {
		t.Fatalf("timeout = %d", got)
	}
	if err := s.SetSwitch("daily_chat_web", false); err != nil || s.DailyChatWeb() {
		t.Fatalf("switch off: %v", err)
	}
	if err := s.SetDailyChatWebAttempts(0); err == nil {
		t.Fatal("zero attempts must be rejected")
	}
	if err := s.SetDailyChatWebAttempts(6); err == nil {
		t.Fatal("six attempts must be rejected")
	}
	if err := s.SetDailyChatWebAttempts(5); err != nil || s.DailyChatWebAttempts() != 5 {
		t.Fatalf("five: %v", err)
	}
	if err := s.SetDailyChatWebTimeout(10); err == nil {
		t.Fatal("a ten-second timeout must be rejected")
	}
	if err := s.SetDailyChatWebTimeout(600); err != nil || s.DailyChatWebTimeout() != 600 {
		t.Fatalf("600: %v", err)
	}
}
