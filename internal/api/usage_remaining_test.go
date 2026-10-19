package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/store/accounts"
	"wb2api/internal/usagedb"
	"wb2api/internal/usageest"
)

func TestRemainingUsageEndpointAndETag(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	db, err := usagedb.Open(dir+"/usage.db", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.Record(usagedb.Event{At: now.Add(-3 * time.Hour), Account: "u1", Model: "m1", Realm: "intl", Total: 100, Outcome: "completed"})
	db.Record(usagedb.Event{At: now.Add(-time.Minute), Account: "u1", Model: "m1", Realm: "intl", Total: 20, Outcome: "completed"})
	db.Flush()

	p := pool.New([]*accounts.Record{accounts.FromMap(map[string]any{
		"uid": "u1", "accessToken": "tok", "realm": "intl", "expiresAt": now.Add(time.Hour).Unix(), "enabled": true,
	}, "", now)})
	est := &usageest.Estimator{DB: db, Pool: p, Dir: dir, Now: func() time.Time { return now }, TTL: time.Hour}
	est.NoteLimit("u1", "m1", "intl", now.Add(-time.Hour), now.Add(-2*time.Hour))

	mux := http.NewServeMux()
	(&Usage{DB: db, Est: est}).Register(mux, func(h http.Handler) http.Handler { return h })

	req := httptest.NewRequest("GET", "/api/v2/usage/remaining", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"rows"`, `"u1"`, `"remaining":80`, `"budget":100`, `"total_remaining":80`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s: %s", want, body)
		}
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	req2 := httptest.NewRequest("GET", "/api/v2/usage/remaining", nil)
	req2.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("conditional request: %d", rec2.Code)
	}
}
