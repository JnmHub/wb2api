package usageest

import (
	"context"
	"errors"
	"testing"
	"time"
	"wb2api/internal/usagedb"
)

func TestStopCancelsManualAndLoopRefreshFlights(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual", true: "background"}[background], func(t *testing.T) {
			entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			e := &Estimator{RefreshTimeout: time.Minute, pointQuery: func(ctx context.Context, _, _ time.Time) ([]usagedb.PairPoint, error) {
				close(entered)
				<-ctx.Done()
				close(cancelled)
				<-release
				return nil, ctx.Err()
			}}
			refreshed := make(chan error, 1)
			if background {
				e.Start(context.Background())
			} else {
				go func() { refreshed <- e.Refresh(context.Background()) }()
			}
			<-entered
			done := make(chan error, 1)
			go func() { done <- e.Stop(context.Background()) }()
			select {
			case <-cancelled:
			case <-time.After(time.Second):
				t.Fatal("Stop did not cancel flight")
			}
			select {
			case <-done:
				t.Fatal("Stop returned before read joined")
			default:
			}
			close(release)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("Stop did not join flight")
			}
			if !background {
				if err := <-refreshed; !errors.Is(err, context.Canceled) {
					t.Fatalf("refresh=%v", err)
				}
			}
			if err := e.Refresh(context.Background()); !errors.Is(err, context.Canceled) {
				t.Fatalf("Refresh after Stop=%v", err)
			}
			if e.ScheduleSnapshot() != nil {
				t.Fatal("cancelled flight published")
			}
		})
	}
}
