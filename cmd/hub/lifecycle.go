package main

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

// requestDrain serializes admission with shutdown. Unlike WaitGroup.Add racing
// Wait at zero, no admitted handler can escape the terminal drained barrier.
// It wraps only Handler, not ResponseWriter, preserving streaming interfaces.
type requestDrain struct {
	mu      sync.Mutex
	stopped bool
	active  int
	done    chan struct{}
}

func newRequestDrain() *requestDrain { return &requestDrain{done: make(chan struct{})} }

func (d *requestDrain) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		if d.stopped {
			d.mu.Unlock()
			http.Error(w, "server shutting down", http.StatusServiceUnavailable)
			return
		}
		d.active++
		d.mu.Unlock()
		defer func() {
			d.mu.Lock()
			d.active--
			if d.stopped && d.active == 0 {
				close(d.done)
			}
			d.mu.Unlock()
		}()
		next.ServeHTTP(w, r)
	})
}

func (d *requestDrain) stop() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.stopped {
		d.stopped = true
		if d.active == 0 {
			close(d.done)
		}
	}
	return d.done
}

// backgroundTasks has static enrollment: main starts all tasks before shutdown
// begins and never calls start concurrently with wait. Internally spawned
// workers need their own stop/join API; wrapping their Start call is not a join.
type backgroundTasks struct{ wg sync.WaitGroup }

func (g *backgroundTasks) start(fn func()) {
	g.wg.Add(1)
	go func() { defer g.wg.Done(); fn() }()
}
func (g *backgroundTasks) wait() { g.wg.Wait() }

type shutdownServer interface {
	Shutdown(context.Context) error
	Close() error
}

// quiesceHTTP keeps storage alive until every possible late usage producer
// has terminated. If a handler ignores cancellation we deliberately keep waiting
// rather than closing the DB underneath it or claiming its usage was persisted.
// report receives all errors; a timeout still force-closes/cancels requests.
func quiesceHTTP(srv shutdownServer, drain *requestDrain, grace time.Duration, cancelRequests func(), report func(error)) {
	done := drain.stop()
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	err := srv.Shutdown(ctx)
	cancel()
	if err != nil {
		report(err)
		cancelRequests()
		if closeErr := srv.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
			report(closeErr)
		}
	}
	cancelRequests()
	// Shutdown does not wait for hijacked connections, and Close does not join
	// handlers. This gate is the terminal barrier for final OnUsage callbacks.
	<-done
}

// finishStorage is the only usage storage finalizer. HTTP must already have
// quiesced. Cancellation and joining precede observation flushing and DB close.
func finishStorage(stop func(), join func(), flush func(context.Context) error, closeDB func() error, report func(error)) {
	stop()
	join()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	if err := flush(ctx); err != nil {
		report(err)
	}
	cancel()
	if err := closeDB(); err != nil {
		report(err)
	}
}
