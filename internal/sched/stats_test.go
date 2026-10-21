package sched

import (
	"testing"
	"time"
)

func TestStatsEscapesSlowAndRecovers(t *testing.T) {
	s := NewStats(15*time.Second, 0.5)
	s.Observe("a", 20*time.Second, false)
	if !s.ShouldEscape("a") {
		t.Fatal("slow account should escape")
	}
	for i := 0; i < 4; i++ {
		s.Observe("a", time.Second, false)
	}
	if s.ShouldEscape("a") {
		t.Fatal("account should recover after good samples")
	}
}

func TestStatsErrorAndClientAbort(t *testing.T) {
	s := NewStats(0, 0.5)
	s.Observe("a", 0, true)
	if !s.ShouldEscape("a") {
		t.Fatal("erroring account should escape")
	}
	s2 := NewStats(0, 0.5)
	s2.Observe("b", 0, Failure("client_aborted"))
	if s2.ShouldEscape("b") {
		t.Fatal("client abort must not count as account failure")
	}
	if !Failure("upstream_aborted") || Failure("completed") || Failure("ok") {
		t.Fatal("outcome classification")
	}
}
