package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wb2api/internal/pricing"
	"wb2api/internal/usagedb"
)

const priceCatalogue = `{"data":[
 {"id":"openai/gpt-6-astra","name":"GPT-6","pricing":{"prompt":"0.00001","completion":"0.00005"}}
]}`

func TestPricingEndpoints(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, priceCatalogue)
	}))
	defer srv.Close()
	store := pricing.Open(dir)
	store.FetchURL = srv.URL

	db, err := usagedb.Open(filepath.Join(dir, "usage.db"), 8)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Record(usagedb.Event{At: time.Now(), Model: "gpt-6-astra", Prompt: 1_000_000, Completion: 1_000_000, Outcome: "completed"})
	db.Flush()

	mux := http.NewServeMux()
	(&Usage{DB: db, Pricing: store}).Register(mux, func(h http.Handler) http.Handler { return h })
	do := func(method, url, body string) (int, string) {
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, url, rd))
		return rec.Code, rec.Body.String()
	}

	if code, body := do("POST", "/api/v2/pricing/refresh", ""); code != 200 || !strings.Contains(body, `"fetched":1`) {
		t.Fatalf("refresh: %d %s", code, body)
	}
	code, body := do("GET", "/api/v2/pricing/cost?range=today", "")
	if code != 200 {
		t.Fatalf("cost: %d %s", code, body)
	}
	if !strings.Contains(body, `"cost_usd":60`) || !strings.Contains(body, `"priced":true`) {
		t.Fatalf("cost body: %s", body)
	}
	code, body = do("POST", "/api/v2/pricing/policy", `{"model":"mine","mode":"manual","or_id":"openai/gpt-6-astra"}`)
	if code != 200 || !strings.Contains(body, `"mine"`) {
		t.Fatalf("policy: %d %s", code, body)
	}
	if code, _ := do("POST", "/api/v2/pricing/policy", `{"model":"","mode":"auto"}`); code != 400 {
		t.Fatalf("empty model should be 400, got %d", code)
	}
	code, body = do("GET", "/api/v2/pricing", "")
	if code != 200 || !strings.Contains(body, `"models":1`) {
		t.Fatalf("status: %d %s", code, body)
	}
}
