package pricing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const catalogue = `{"data":[
 {"id":"openai/gpt-6-astra","name":"GPT-6 Astra","pricing":{"prompt":"0.00001","completion":"0.00005","input_cache_read":"0.000001"}},
 {"id":"z-ai/glm-5.3-flash","name":"GLM 5.3 Flash","pricing":{"prompt":"0.0000001","completion":"0.0000005"}},
 {"id":"deepseek/deepseek-v4.1-flash","name":"DeepSeek 4.1 Flash","pricing":{"prompt":"0.00000003","completion":"0.0000005","input_cache_read":"0.00000001"}}
]}`

func TestRefreshAndPrice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(catalogue))
	}))
	defer srv.Close()
	dir := t.TempDir()
	s := Open(dir)
	s.FetchURL = srv.URL
	n, err := s.Refresh(context.Background())
	if err != nil || n != 3 {
		t.Fatalf("%d %v", n, err)
	}
	price, source, ok := s.Price("gpt-6-astra")
	if !ok || source != "alias" || price.Output != 50 {
		t.Fatalf("%+v %s %v", price, source, ok)
	}
	price, source, ok = s.Price("deepseek/deepseek-v4.1-flash")
	if !ok || source != "exact" || price.CacheRead != 0.01 {
		t.Fatalf("exact: %+v %s", price, source)
	}
	// Channel suffix falls back to the unique base.
	price, source, ok = s.Price("deepseek-v4.1-flash-lkeap")
	if !ok || source != "suffix" {
		t.Fatalf("suffix: %+v %s %v", price, source, ok)
	}
	// Reload sees the persisted snapshot.
	s2 := Open(dir)
	if _, _, ok := s2.Price("glm-5.3-flash"); !ok {
		t.Fatal("snapshot was not persisted")
	}
}

func TestCostUsesCacheReadAndMiss(t *testing.T) {
	s := Open(t.TempDir())
	s.mu.Lock()
	s.snap = Snapshot{USDCNY: 7, Models: map[string]Model{
		"x/y": {ID: "x/y", Price: Price{Input: 2, Output: 10, CacheRead: 0.5}},
	}}
	s.mu.Unlock()
	cny, usd, ok := s.Cost("y", 1_000_000, 1_000_000, 400_000, time.Now().Unix())
	if !ok {
		t.Fatal("not priced")
	}
	// 600k miss * 2 + 400k cached * 0.5 + 1M output * 10 = 11.4 USD
	if usd != 11.4 || cny != 79.8 {
		t.Fatalf("usd=%v cny=%v", usd, cny)
	}
}

func TestPoliciesOverrideAuto(t *testing.T) {
	s := Open(t.TempDir())
	s.mu.Lock()
	s.snap = Snapshot{USDCNY: 7.1, Models: map[string]Model{
		"a/b": {ID: "a/b", Price: Price{Input: 1, Output: 2}},
	}}
	s.mu.Unlock()
	if _, _, ok := s.Price("mine"); ok {
		t.Fatal("unknown model must not be priced")
	}
	if err := s.SetPolicy("mine", Policy{Mode: "manual", ORID: "a/b"}); err != nil {
		t.Fatal(err)
	}
	if p, src, ok := s.Price("mine"); !ok || src != "policy" || p.Output != 2 {
		t.Fatalf("mapped: %+v %s", p, src)
	}
	in := 9.0
	if err := s.SetPolicy("mine", Policy{Mode: "manual", Input: &in}); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := s.Price("mine"); p.Input != 9 {
		t.Fatalf("explicit override: %+v", p)
	}
	if err := s.SetPolicy("mine", Policy{Mode: "ignore"}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Price("mine"); ok {
		t.Fatal("ignored model must not be priced")
	}
	if err := s.SetPolicy("mine", Policy{Mode: ""}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Policies()["mine"]; ok {
		t.Fatal("empty mode clears the policy")
	}
}

func TestPoliciesPersistAndValidate(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	if err := s.SetPolicy("m", Policy{Mode: "manual", Output: ptr(1.5), Note: "hand"}); err != nil {
		t.Fatal(err)
	}
	s2 := Open(dir)
	if p := s2.Policies()["m"]; p.Output == nil || *p.Output != 1.5 || p.Note != "hand" {
		t.Fatalf("%+v", p)
	}
	if err := s.SetPolicy("", Policy{Mode: "auto"}); err == nil {
		t.Fatal("empty model must fail")
	}
	if err := s.SetPolicy("m", Policy{Mode: "bogus"}); err == nil {
		t.Fatal("unknown mode must fail")
	}
	if err := s.SetPolicy("m", Policy{Mode: "manual"}); err == nil {
		t.Fatal("manual without price must fail")
	}
	neg := -1.0
	if err := s.SetPolicy("m", Policy{Mode: "manual", Input: &neg}); err == nil {
		t.Fatal("negative price must fail")
	}
	if _, err := os.Stat(filepath.Join(dir, "pricing_policies.json")); err != nil {
		t.Fatal(err)
	}
}

func ptr(v float64) *float64 { return &v }
