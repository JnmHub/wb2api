package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/activity"
	"wb2api/internal/ops"
	"wb2api/internal/pool"
	"wb2api/internal/store/accounts"
	"wb2api/internal/store/settings"
	"wb2api/internal/upstream"
)

func newTasksFixture(t *testing.T) (*http.ServeMux, *pool.Pool) {
	t.Helper()
	claimed := &atomic.Bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/activity/growth/tasks":
			status := "completed"
			if claimed.Load() {
				status = "claimed"
			}
			io.WriteString(w, `{"data":{"tasks":[
			  {"task_code":"create_canvas","title":"制作画布","accept_status":"`+status+`","task_type":"single","progress":{"current":1,"target":1}}]}}`)
		case "/activity/growth/tasks/create_canvas/claim":
			claimed.Store(true)
			io.WriteString(w, `{"code":0,"data":{"credit":300}}`)
		case "/activity/growth/buddy/travel/status":
			io.WriteString(w, `{"code":0,"data":{"state":"traveling"}}`)
		case "/v2/activity/growth/energy":
			io.WriteString(w, `{"data":{"balance":12}}`)
		case "/activity/growth/streak":
			io.WriteString(w, `{"data":{"streak":{"days":3}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	p := pool.New([]*accounts.Record{accounts.FromMap(map[string]any{
		"uid": "cn-1", "accessToken": "tok", "realm": "cn", "nickname": "国内一号",
		"expiresAt": time.Now().Add(24 * time.Hour).Unix(), "enabled": true,
	}, "", time.Now())})
	client := &ops.Client{Pool: p, Up: upstream.NewManager(upstream.DefaultTimeouts()), Settings: settings.Open(dir),
		Activity: &activity.Log{Dir: dir}, BaseOverride: srv.URL, Pause: func(time.Duration) {}}
	mux := http.NewServeMux()
	(&Operations{Ops: client, Pool: p}).Register(mux, func(h http.Handler) http.Handler { return h })
	return mux, p
}

func call(t *testing.T, mux *http.ServeMux, method, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestTasksSnapshot(t *testing.T) {
	mux, _ := newTasksFixture(t)
	code, body := call(t, mux, "GET", "/api/v2/tasks", "")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	for _, want := range []string{`"tasks"`, `"create_canvas"`, `"summary"`, `"energy":12`, `"accounts"`} {
		if !strings.Contains(body, want) {
			t.Errorf("snapshot missing %s: %s", want, body)
		}
	}
}

func TestTasksRunClaimsAndReportsCredit(t *testing.T) {
	mux, _ := newTasksFixture(t)
	code, body := call(t, mux, "POST", "/api/v2/tasks/run", `{}`)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	for _, want := range []string{`"credit_added":300`, `"accounts_count":1`, "猫猫日常"} {
		if !strings.Contains(body, want) {
			t.Errorf("run missing %s: %s", want, body)
		}
	}
}

func TestTasksTravelAndStreak(t *testing.T) {
	mux, _ := newTasksFixture(t)
	code, body := call(t, mux, "POST", "/api/v2/tasks/travel", `{}`)
	if code != 200 || !strings.Contains(body, "旅行途中") {
		t.Fatalf("%d %s", code, body)
	}
	code, body = call(t, mux, "POST", "/api/v2/tasks/streak-bonus", `{}`)
	if code != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("%d %s", code, body)
	}
}
