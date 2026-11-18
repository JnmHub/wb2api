package openai

import (
	"testing"
	"time"
)

func TestSizeGateRecordsAndBlocks(t *testing.T) {
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	var g sizeGate

	big := sizeGateMinBytes + 1024
	if blocked, _ := g.blocked("hw-1", big, now); blocked {
		t.Fatal("nothing measured yet")
	}
	g.record("hw-1", big, now, "HTTP 413")

	if blocked, reason := g.blocked("hw-1", big, now); !blocked || reason == "" {
		t.Fatalf("same size must be blocked: %v %q", blocked, reason)
	}
	if blocked, _ := g.blocked("hw-1", big+4096, now); !blocked {
		t.Fatal("a larger body must be blocked too")
	}
	if blocked, _ := g.blocked("hw-1", big-4096, now); blocked {
		t.Fatal("a smaller body may still fit")
	}
	if blocked, _ := g.blocked("hw-2", big, now); blocked {
		t.Fatal("the wall is per account")
	}
	// Expiry: the wall is only trusted briefly.
	if blocked, _ := g.blocked("hw-1", big, now.Add(sizeGateTTL+time.Second)); blocked {
		t.Fatal("the wall must expire")
	}
}

// A small body is never gated: 001001005 is also the generic invalid-parameter
// code, and blocking on it would refuse valid requests.
func TestSizeGateIgnoresSmallBodies(t *testing.T) {
	now := time.Now()
	var g sizeGate
	g.record("hw-1", sizeGateMinBytes-1, now, "HTTP 400")
	if blocked, _ := g.blocked("hw-1", sizeGateMinBytes+1, now); blocked {
		t.Fatal("a sub-threshold measurement must not create a wall")
	}
	if blocked, _ := g.blocked("hw-1", 1024, now); blocked {
		t.Fatal("small bodies are never gated")
	}
}
