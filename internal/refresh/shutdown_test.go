package refresh

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream/codearts"
	"wb2api/internal/upstream/raccoon"
)

func TestStopAndWaitDrainsDetachedRotatingRefresh(t *testing.T) {
	for _, channel := range []string{realm.CodeArts, realm.Raccoon} {
		t.Run(channel, func(t *testing.T) {
			var record *accounts.Record
			path := raccoon.RefreshPath
			reply := "{\"code\":0,\"data\":{\"access_token\":\"new-at\",\"refresh_token\":\"rt-rotated\"}}"
			if channel == realm.CodeArts {
				key, err := codearts.NewDPoPKey()
				if err != nil {
					t.Fatal(err)
				}
				record = codeartsRecord(t, key)
				path = codearts.TokenPath
				reply = fmt.Sprintf("{\"refresh_token\":\"rt-rotated\",\"credentials\":{\"access_key_id\":\"AK-NEW\",\"secret_access_key\":\"SK-NEW\",\"security_token\":\"STS-NEW\",\"expiration\":%q}}",
					time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			} else {
				record = &accounts.Record{UID: "rc-stop", Realm: realm.Raccoon, AccessToken: "old-at", RefreshToken: "old-rt"}
			}

			started, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != path {
					t.Errorf("unexpected exchange path: %s", req.URL.Path)
				}
				if hits.Add(1) == 1 {
					close(started)
				}
				<-release
				fmt.Fprint(w, reply)
			}))
			defer server.Close()
			defer releaseOnce.Do(func() { close(release) })

			p := pool.New([]*accounts.Record{record})
			acc := p.Get(record.UID)
			var saved atomic.Bool
			r := &Refresher{Pool: p, BaseURL: func(*pool.Account) string { return server.URL },
				Persist: func(next *accounts.Record) error {
					if next.RefreshToken == "rt-rotated" {
						saved.Store(true)
					}
					return nil
				}}

			ctx, deadline := context.WithTimeout(t.Context(), 5*time.Second)
			defer deadline()
			caller, cancel := context.WithCancel(ctx)
			defer cancel()
			clientResult := make(chan error, 1)
			go func() { clientResult <- r.Refresh(caller, acc) }()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("refresh did not reach upstream")
			}
			cancel()
			select {
			case err := <-clientResult:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("caller did not return on cancellation: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("caller remained blocked during the refresh")
			}

			shutdown := make(chan struct{})
			go func() {
				r.StopAndWait()
				close(shutdown)
			}()
			poll := time.NewTicker(time.Millisecond)
			defer poll.Stop()
			for {
				r.mu.Lock()
				stopped := r.stopped
				r.mu.Unlock()
				if stopped {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("shutdown did not fence new refreshes")
				case <-poll.C:
				}
			}
			select {
			case <-shutdown:
				t.Fatal("shutdown returned before the blocked exchange completed")
			default:
			}
			if err := r.Refresh(ctx, acc); !errors.Is(err, ErrStopping) {
				t.Fatalf("refresh during shutdown = %v, want ErrStopping", err)
			}

			releaseOnce.Do(func() { close(release) })
			select {
			case <-shutdown:
			case <-ctx.Done():
				t.Fatal("shutdown did not join the completed exchange")
			}
			if hits.Load() != 1 || !saved.Load() || acc.Record().RefreshToken != "rt-rotated" {
				t.Fatalf("shutdown lost the rotated credential: upstream calls=%d, saved=%v", hits.Load(), saved.Load())
			}
		})
	}
}
