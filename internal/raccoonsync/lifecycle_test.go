package raccoonsync

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/refresh"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream/raccoon"
)

func TestDiscoveryDoesNotOverwriteNewCredentialOrRecreateDeletedAccount(t *testing.T) {
	for _, mode := range []string{"disable", "replace", "delete"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			dir := t.TempDir()
			initial, err := accounts.SaveReconciled(dir, raccoonAccount("rc-race", now.Add(time.Hour)), now)
			if err != nil {
				t.Fatal(err)
			}
			p := pool.New([]*accounts.Record{initial})
			acc := p.Get(initial.UID)
			started, release := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-release
				fmt.Fprint(w, "{\"code\":0,\"data\":{\"categories\":[{\"type\":\"chat\",\"models\":[{\"name\":\"sn-glm-5-3\",\"visible\":true}]}]}}")
			}))
			defer server.Close()
			svc := &Service{Pool: p, Client: &raccoon.Client{LlmBase: server.URL}, Persist: func(record *accounts.Record) error {
				committed, err := accounts.SaveReconciled(dir, record, now)
				if committed != nil {
					p.ApplyChanges([]*accounts.Record{committed}, nil)
				}
				return err
			}}
			done := make(chan Discovery, 1)
			go func() { done <- svc.Discover(context.Background(), acc) }()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				close(release)
				t.Fatal("catalogue request did not start")
			}
			if mode == "delete" {
				p.Remove(acc.UID)
				err = accounts.Delete(initial)
			} else {
				next := *initial
				next.Enabled = false
				if mode == "replace" {
					next.AccessToken, next.RefreshToken = "new-at", "new-rt"
				}
				err = svc.Persist(&next)
			}
			close(release)
			result := <-done
			if err != nil {
				t.Fatal(err)
			}
			records, bad := accounts.Load(dir, now)
			if len(bad) != 0 {
				t.Fatal(bad)
			}
			if mode == "delete" {
				if !errors.Is(result.Err, accounts.ErrDeleted) || len(records) != 0 || p.Get(acc.UID) != nil {
					t.Fatalf("deleted account came back: err=%v records=%d", result.Err, len(records))
				}
				return
			}
			if len(records) != 1 || records[0].IsEnabled() || acc.Enabled() {
				t.Fatalf("in-flight discovery changed disabled state: err=%v", result.Err)
			}
			if mode == "replace" {
				if !errors.Is(result.Err, accounts.ErrConflict) || records[0].AccessToken != "new-at" ||
					records[0].RefreshToken != "new-rt" || acc.Record().RefreshToken != "new-rt" {
					t.Fatalf("in-flight discovery changed replacement credential: err=%v", result.Err)
				}
				return
			}
			records[0].Enabled = true
			if result.Err != nil || raccoon.ModelsDiscoveredAt(records[0]) == 0 ||
				(&Scanner{Service: svc}).ModelRefreshDue(pool.New(records).Get(acc.UID), now.Add(time.Minute)) {
				t.Fatalf("metadata or its persisted timestamp was lost: err=%v", result.Err)
			}
		})
	}
}

func TestCheckinRefreshesRejectedTokenAndAllowsExplicitDisabledAccount(t *testing.T) {
	now := time.Now()
	initial := raccoonAccount("rc-checkin", now.Add(time.Hour))
	initial.Enabled = false
	p := pool.New([]*accounts.Record{initial})
	acc := p.Get(initial.UID)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer new-at" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, "{\"code\":0,\"data\":{\"items\":[]}}")
	}))
	defer server.Close()
	attempts := 0
	svc := &Service{Pool: p, Client: &raccoon.Client{Origin: server.URL},
		RefreshObserved: func(ctx context.Context, a *pool.Account, old *accounts.Record) error {
			if old.AccessToken != "t" {
				t.Errorf("did not refresh the rejected token generation")
			}
			attempts++
			p.Update(a.UID, func(next *accounts.Record) { next.AccessToken = "new-at" })
			return nil
		}}
	if _, err := svc.Checkin(t.Context(), acc); !errors.Is(err, refresh.ErrRaccoonDisabled) || attempts != 0 {
		t.Fatalf("disabled automatic check-in returned %v, refreshes=%d", err, attempts)
	}
	if _, err := svc.Checkin(refresh.WithManualRaccoon(t.Context(), acc.UID), acc); err != nil ||
		attempts != 1 || acc.Enabled() {
		t.Fatalf("manual check-in returned %v, refreshes=%d enabled=%v", err, attempts, acc.Enabled())
	}
}
