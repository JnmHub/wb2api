package api

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestOpsGateLimitsAndReconfigures(t *testing.T) {
	g := NewOpsGate(1, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	started := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = g.Do(ctx, func() error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	short, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	if err := g.Do(short, func() error { return nil }); err == nil {
		t.Fatal("a full gate must block the second operation")
	}

	// Raising the limit while the first slot is still held must let new work in.
	g.Configure(2, 0)
	entered := make(chan struct{}, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = g.Do(ctx, func() error { entered <- struct{}{}; return nil })
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("gate did not admit work after the limit was raised")
		}
	}
	close(release)
	wg.Wait()
}
