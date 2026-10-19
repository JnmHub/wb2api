package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wb2api/internal/store/settings"
	"wb2api/internal/updates"
)

func TestUpdatesEndpoint(t *testing.T) {
	store := settings.Open(t.TempDir())
	if err := store.SetUpdateCheckEnabled(true); err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"tag_name":"v2.1.0","html_url":"https://github.com/x/y/releases/tag/v2.1.0"}]`)
	}))
	defer up.Close()
	checker := &updates.Checker{Current: "2.0.0", Enabled: store.UpdateCheckEnabled, Store: store,
		APIBase: up.URL, Client: &http.Client{Timeout: 2 * time.Second}}
	mux := http.NewServeMux()
	(&Updates{Checker: checker}).Register(mux, func(h http.Handler) http.Handler { return h })

	req := httptest.NewRequest("GET", "/api/v2/updates", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"current_version":"2.0.0"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest("POST", "/api/v2/updates/check", strings.NewReader("{}"))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `"latest_version":"2.1.0"`) || !strings.Contains(body, `"update_available":true`) {
		t.Fatalf("%d %s", rec.Code, body)
	}
}
