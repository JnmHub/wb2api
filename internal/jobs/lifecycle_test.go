package jobs

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestStopCancelsJoinsAndDrainsFinalSnapshot(t *testing.T) {
	store := newMemStore()
	m := &Manager{Store: store, Workers: 1}
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	j := m.Start("slow", []string{"u1"}, func(ctx context.Context, _ string) error {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		return ctx.Err()
	})
	<-started
	done := make(chan error, 1)
	go func() { done <- m.Stop(context.Background()) }()
	<-cancelled
	select {
	case <-done:
		t.Fatal("Stop returned before item completed")
	default:
	}
	if got := m.Start("rejected", nil, func(context.Context, string) error { t.Error("started after Stop"); return nil }); got.State != Cancelled {
		t.Fatalf("admission state=%s", got.State)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop did not drain")
	}
	got, ok := store.get(j.ID)
	if !ok || got.State != Cancelled || got.Done != 1 || got.Finished == 0 {
		t.Fatalf("terminal snapshot=%+v exists=%v", got, ok)
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type blockingSyncStore struct {
	*memStore
	entered, exited chan struct{}
}

func (s *blockingSyncStore) Load(ctx context.Context, _ int) ([]Job, error) {
	close(s.entered)
	<-ctx.Done()
	close(s.exited)
	return nil, ctx.Err()
}
func TestStopCancelsAndJoinsBackgroundSync(t *testing.T) {
	store := &blockingSyncStore{memStore: newMemStore(), entered: make(chan struct{}), exited: make(chan struct{})}
	m := &Manager{Store: store}
	m.StartSync(context.Background())
	<-store.entered
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-store.exited:
	default:
		t.Fatal("sync not joined")
	}
}

func TestStopDeadlineRequiresLaterJoin(t *testing.T) {
	m := &Manager{Workers: 1}
	entered, release := make(chan struct{}), make(chan struct{})
	m.Start("ignores cancellation", []string{"u"}, func(context.Context, string) error { close(entered); <-release; return nil })
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := m.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	close(release)
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}
