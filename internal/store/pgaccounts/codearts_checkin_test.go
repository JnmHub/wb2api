package pgaccounts

import (
	"context"
	"testing"
)

func TestCodeArtsCheckinLockSpansReplicas(t *testing.T) {
	ctx, db := syncTestDB(t)
	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- WithCodeArtsCheckinLock(ctx, db, "hw-1", func(ctx context.Context) error {
			close(locked)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	defer close(release)
	select {
	case <-locked:
	case err := <-done:
		t.Fatalf("first lock failed: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := WithCodeArtsCheckinLock(ctx, db, "hw-1", func(context.Context) error {
		t.Error("two replica connections held the lock for one account")
		return nil
	}); err == nil {
		t.Fatal("the lock contention was not reported")
	}
	if err := WithCodeArtsCheckinLock(ctx, db, "hw-2", func(context.Context) error { return nil }); err != nil {
		t.Fatalf("another account could not run: %v", err)
	}
}
