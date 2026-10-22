package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"
)

type fakeShutdownServer struct {
	shutdown func(context.Context) error
	close    func() error
}

func (s fakeShutdownServer) Shutdown(ctx context.Context) error { return s.shutdown(ctx) }
func (s fakeShutdownServer) Close() error                       { return s.close() }

func TestShutdownTimeoutJoinsLateUsageBeforeStorageClose(t *testing.T) {
	requests, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	drain := newRequestDrain()
	entered, cancelled, release, handlerDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var events []string
	record := func(s string) { mu.Lock(); events = append(events, s); mu.Unlock() }
	handler := drain.wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-requests.Done()
		close(cancelled)
		<-release // final usage callback can happen after Shutdown and Close return
		record("late usage")
	}))
	go func() {
		defer close(handlerDone)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	}()
	<-entered
	closed := make(chan struct{})
	srv := fakeShutdownServer{shutdown: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, close: func() error { close(closed); return nil }}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		quiesceHTTP(srv, drain, time.Millisecond, cancelRequests, func(err error) {
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("shutdown error=%v", err)
			}
		})
		finishStorage(func() { record("stop") }, func() { record("join") }, func(context.Context) error { record("flush"); return nil }, func() error { record("close"); return nil }, func(err error) { t.Errorf("finalize: %v", err) })
	}()
	<-closed
	<-cancelled
	select {
	case <-finished:
		t.Fatal("storage finalized before late handler joined")
	default:
	}
	denied := httptest.NewRecorder()
	handler.ServeHTTP(denied, httptest.NewRequest("GET", "/", nil))
	if denied.Code != http.StatusServiceUnavailable {
		t.Fatalf("admission=%d", denied.Code)
	}
	close(release)
	<-handlerDone
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not join handler")
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"late usage", "stop", "join", "flush", "close"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("order=%v want %v", events, want)
	}
}

func TestSuccessfulShutdownCancelsHijackedStyleHandlerBeforeJoin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	drain := newRequestDrain()
	entered, handlerDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(handlerDone)
		drain.wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(entered); <-ctx.Done() })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	}()
	<-entered
	done := make(chan struct{})
	go func() {
		defer close(done)
		quiesceHTTP(fakeShutdownServer{shutdown: func(context.Context) error { return nil }, close: func() error { t.Error("unexpected forced close"); return nil }}, drain, time.Second, cancel, func(err error) { t.Error(err) })
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("successful Shutdown must cancel excluded/hijacked handler before joining")
	}
	<-handlerDone
}

func TestFinishStorageWaitsForBackgroundAndClosesAfterFlushFailure(t *testing.T) {
	release, entered, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var events []string
	wantErr := errors.New("unpersisted observations")
	go func() {
		defer close(done)
		finishStorage(func() { events = append(events, "stop") }, func() { close(entered); <-release; events = append(events, "joined") }, func(context.Context) error { events = append(events, "flush"); return wantErr }, func() error { events = append(events, "close"); return nil }, func(err error) {
			if !errors.Is(err, wantErr) {
				t.Errorf("err=%v", err)
			}
			events = append(events, "reported")
		})
	}()
	<-entered
	select {
	case <-done:
		t.Fatal("closed while background reader active")
	default:
	}
	close(release)
	<-done
	if want := []string{"stop", "joined", "flush", "reported", "close"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v", events)
	}
}
