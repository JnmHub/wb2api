package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/ops"
	"wb2api/internal/pool"
	"wb2api/internal/raccoonsync"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream/raccoon"
)

func TestProfileActionsKeepCredentialsInTheirChannel(t *testing.T) {
	now := time.Now()
	p := pool.New([]*accounts.Record{
		{UID: "rc", Realm: realm.Raccoon, AccessToken: "rc-synthetic", ExpiresAt: now.Add(time.Hour).Unix()},
		{UID: "hw", Realm: realm.CodeArts, AccessToken: "hw-synthetic"},
	})
	var rcCalls, wbCalls atomic.Int32
	rc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rcCalls.Add(1)
		if r.URL.Path != raccoon.UserInfoPath || r.Header.Get("Authorization") != "Bearer rc-synthetic" {
			t.Errorf("unexpected nickname request at %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"code":0,"data":{"nickname":"小浣熊测试"}}`)
	}))
	defer rc.Close()
	wb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wbCalls.Add(1)
		http.Error(w, "foreign credential", http.StatusBadRequest)
	}))
	defer wb.Close()
	buddy := &ops.Client{BaseOverride: wb.URL}
	raccoons := &raccoonsync.Service{Pool: p, Client: &raccoon.Client{HTTP: rc.Client(), AuthBase: rc.URL}}
	if profile := syncAccountProfile(t.Context(), p.Get("rc"), buddy, raccoons); !profile.OK || p.Get("rc").Record().Nickname != "小浣熊测试" {
		t.Fatalf("Raccoon nickname did not reach its own upstream and update the account: %v", profile)
	}
	if profile := syncAccountProfile(t.Context(), p.Get("hw"), buddy, raccoons); profile.OK {
		t.Fatal("CodeArts used a WorkBuddy profile operation")
	}
	if rcCalls.Load() != 1 || wbCalls.Load() != 0 {
		t.Fatalf("wrong provider calls: Raccoon=%d WorkBuddy=%d", rcCalls.Load(), wbCalls.Load())
	}
}
