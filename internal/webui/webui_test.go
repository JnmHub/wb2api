package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func app(t *testing.T, withIndex bool) *Handler {
	t.Helper()
	fsys := fstest.MapFS{
		"assets/app-abc123.js":    {Data: []byte("console.log('app')")},
		"assets/app-abc123.js.br": {Data: []byte("BR")},
		"assets/app-abc123.js.gz": {Data: []byte("GZ")},
		"assets/style-1.css":      {Data: []byte("body{}")},
		"favicon.svg":             {Data: []byte("<svg/>")},
		".keep":                   {Data: nil},
	}
	if withIndex {
		fsys["index.html"] = &fstest.MapFile{Data: []byte("<!doctype html><div id=root></div>")}
	}
	h, err := New(fsys)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func get(h http.Handler, path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestSPAFallbackAndRealNotFound(t *testing.T) {
	h := app(t, true)
	for _, p := range []string{"/", "/accounts", "/usage/today", "/some/deep/route"} {
		w := get(h, p, nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "id=root") {
			t.Errorf("%s -> %d", p, w.Code)
		}
	}
	if w := get(h, "/assets/missing.js", nil); w.Code != 404 {
		t.Errorf("a missing asset is a real 404, got %d", w.Code)
	}
}

func TestAPIPathsNeverGetTheHTMLShell(t *testing.T) {
	h := app(t, true)
	for _, p := range []string{"/api/v2/nope", "/v1/whatever", "/api"} {
		w := get(h, p, nil)
		if w.Code != 404 || !strings.Contains(w.Header().Get("Content-Type"), "json") || strings.Contains(w.Body.String(), "<!doctype") {
			t.Errorf("%s -> %d %s", p, w.Code, w.Body.String())
		}
	}
}

func TestCachingAndETag(t *testing.T) {
	h := app(t, true)
	a := get(h, "/assets/app-abc123.js", nil)
	if a.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("hashed assets are immutable: %q", a.Header().Get("Cache-Control"))
	}
	idx := get(h, "/", nil)
	if idx.Header().Get("Cache-Control") != "no-cache" || idx.Header().Get("ETag") == "" {
		t.Errorf("index must be revalidated: %v", idx.Header())
	}
	if w := get(h, "/", map[string]string{"If-None-Match": idx.Header().Get("ETag")}); w.Code != 304 || w.Body.Len() != 0 {
		t.Errorf("conditional request: %d", w.Code)
	}
}

func TestPrecompressedVariants(t *testing.T) {
	h := app(t, true)
	cases := []struct{ accept, enc, body string }{
		{"gzip, deflate, br", "br", "BR"},
		{"gzip", "gzip", "GZ"},
		{"br;q=0, gzip", "gzip", "GZ"},
		{"", "", "console.log('app')"},
		{"identity", "", "console.log('app')"},
	}
	for _, c := range cases {
		w := get(h, "/assets/app-abc123.js", map[string]string{"Accept-Encoding": c.accept})
		if w.Header().Get("Content-Encoding") != c.enc || w.Body.String() != c.body {
			t.Errorf("Accept-Encoding %q -> enc=%q body=%q", c.accept, w.Header().Get("Content-Encoding"), w.Body.String())
		}
		if w.Header().Get("Vary") != "Accept-Encoding" {
			t.Error("Vary missing")
		}
	}
	if w := get(h, "/assets/style-1.css", map[string]string{"Accept-Encoding": "br, gzip"}); w.Header().Get("Content-Encoding") != "" {
		t.Error("no .br exists for the css")
	}
	if ct := get(h, "/assets/app-abc123.js", nil).Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("js content type %q", ct)
	}
}

func TestSecurityHeaders(t *testing.T) {
	w := get(app(t, true), "/", nil)
	for _, k := range []string{"X-Content-Type-Options", "Content-Security-Policy", "X-Frame-Options", "Referrer-Policy"} {
		if w.Header().Get(k) == "" {
			t.Errorf("missing %s", k)
		}
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'self'") {
		t.Error("CSP must restrict sources to self")
	}
}

func TestMissingBuildShowsInstructions(t *testing.T) {
	w := get(app(t, false), "/", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "npm run build") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestPathTraversalIsContained(t *testing.T) {
	h := app(t, true)
	for _, p := range []string{"/../../etc/passwd", "/assets/../../x", "/%2e%2e/%2e%2e/secret"} {
		w := get(h, p, nil)
		if strings.Contains(w.Body.String(), "root:") {
			t.Fatalf("%s leaked", p)
		}
	}
}

func TestNonGetRejected(t *testing.T) {
	w := httptest.NewRecorder()
	app(t, true).ServeHTTP(w, httptest.NewRequest("POST", "/", nil))
	if w.Code != 405 {
		t.Fatal(w.Code)
	}
}

func TestEmbeddedBuildLoads(t *testing.T) {
	if _, err := New(Dist()); err != nil {
		t.Fatal(err)
	}
}
