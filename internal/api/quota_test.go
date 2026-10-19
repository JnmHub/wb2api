package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/quota"
	"wb2api/internal/store/accounts"
)

type scanProber struct{ calls atomic.Int32 }

func (s *scanProber) ProbeModel(ctx context.Context, acc *pool.Account, model string) (bool, time.Time, string) {
	s.calls.Add(1)
	return true, time.Time{}, ""
}

func TestQuotaScanEndpoints(t *testing.T) {
	dir := t.TempDir()
	p := pool.New([]*accounts.Record{accounts.FromMap(map[string]any{
		"uid": "u1", "accessToken": "tok", "realm": "intl",
		"expiresAt": time.Now().Add(time.Hour).Unix(), "enabled": true,
	}, "", time.Now())})
	prober := &scanProber{}
	scanner := &quota.Scanner{Dir: dir, Models: func() []string { return []string{"m1"} }, Prober: prober,
		Windows: &quota.Windows{Dir: dir}}
	mux := http.NewServeMux()
	(&Operations{Pool: p, Scanner: scanner}).Register(mux, func(h http.Handler) http.Handler { return h })

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v2/quota-scan", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"enabled":true`) || !strings.Contains(rec.Body.String(), `"m1"`) {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v2/quota-scan/run", strings.NewReader("{}")))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"sent":1`) {
		t.Fatalf("POST: %d %s", rec.Code, rec.Body.String())
	}
	if prober.calls.Load() != 1 {
		t.Fatalf("probes %d", prober.calls.Load())
	}
}
