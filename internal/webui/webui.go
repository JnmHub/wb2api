// Package webui serves the dashboard single-page app that is embedded into the
// binary (built from ./web into internal/webui/dist).
//
// Assets are served pre-compressed (brotli / gzip files produced at build time)
// with long-lived caching for content-hashed files; index.html is revalidated
// with an ETag so a new release is picked up at once and a repeat visit costs a
// 304. The legacy dashboard was one 855 KB uncompressed HTML file re-read from
// disk on every request.
package webui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

//go:embed all:dist
var embedded embed.FS

// Dist returns the embedded build output.
func Dist() fs.FS {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}

type file struct {
	data []byte
	etag string
	ct   string
	br   []byte
	gz   []byte
}

// Handler serves a single-page app from a file system. Requests under /api/ and
// /v1/ that reach it are unknown API routes and get a JSON 404, never the HTML
// shell.
type Handler struct {
	files map[string]*file
	index *file
}

func init() {
	// Go's built-in table can be platform dependent for these.
	_ = mime.AddExtensionType(".js", "text/javascript; charset=utf-8")
	_ = mime.AddExtensionType(".css", "text/css; charset=utf-8")
	_ = mime.AddExtensionType(".svg", "image/svg+xml")
	_ = mime.AddExtensionType(".json", "application/json")
}

// New loads every file of fsys into memory (a few hundred KB) so serving never
// touches the disk.
func New(fsys fs.FS) (*Handler, error) {
	h := &Handler{files: map[string]*file{}}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasSuffix(p, ".br") || strings.HasSuffix(p, ".gz") || path.Base(p) == ".keep" {
			return nil
		}
		raw, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		f := &file{data: raw, ct: mime.TypeByExtension(path.Ext(p))}
		if f.ct == "" {
			f.ct = "application/octet-stream"
		}
		sum := sha256.Sum256(raw)
		f.etag = `"` + hex.EncodeToString(sum[:8]) + `"`
		f.br, _ = fs.ReadFile(fsys, p+".br")
		f.gz, _ = fs.ReadFile(fsys, p+".gz")
		h.files[p] = f
		return nil
	})
	if err != nil {
		return nil, err
	}
	h.index = h.files["index.html"]
	return h, nil
}

const placeholder = `<!doctype html><meta charset="utf-8"><title>Hub</title>
<body style="font:15px system-ui;max-width:640px;margin:12vh auto;padding:0 20px">
<h2>前端还没有构建</h2>
<p>网关 API 已经在运行，但这个二进制里没有内嵌面板。请先构建前端再重新编译：</p>
<pre>cd web && npm ci && npm run build
cd .. && go build ./cmd/hub</pre>`

const apiNotFound = `{"error":{"message":"not found","type":"invalid_request_error"}}` + "\n"

func accepts(r *http.Request, enc string) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(name), enc) {
			return !strings.Contains(strings.ReplaceAll(params, " ", ""), "q=0") || strings.Contains(params, "q=0.")
		}
	}
	return false
}

func security(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'")
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := path.Clean("/" + r.URL.Path)
	if p == "/api" || strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/v1/") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(apiNotFound))
		return
	}
	security(w)
	name := strings.TrimPrefix(p, "/")
	f := h.files[name]
	immutable := false
	if f == nil {
		// Unknown file-looking paths are real 404s; everything else is an app route.
		if name != "" && strings.Contains(path.Base(name), ".") {
			http.NotFound(w, r)
			return
		}
		f = h.index
	} else if strings.HasPrefix(name, "assets/") {
		immutable = true
	}
	if f == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(placeholder))
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", f.ct)
	hd.Set("Vary", "Accept-Encoding")
	if immutable {
		hd.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		hd.Set("Cache-Control", "no-cache")
	}
	hd.Set("ETag", f.etag)
	if r.Header.Get("If-None-Match") == f.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := f.data
	switch {
	case f.br != nil && accepts(r, "br"):
		hd.Set("Content-Encoding", "br")
		body = f.br
	case f.gz != nil && accepts(r, "gzip"):
		hd.Set("Content-Encoding", "gzip")
		body = f.gz
	}
	hd.Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}
