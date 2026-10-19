package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"wb2api/internal/logbuf"
)

// Logs serves the in-memory log ring.
type Logs struct{ Ring *logbuf.Ring }

func (l *Logs) query(r *http.Request) logbuf.Query {
	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	return logbuf.Query{After: after, Level: q.Get("level"), Tag: q.Get("tag"), Text: q.Get("q"), Limit: limit}
}

func (l *Logs) list(w http.ResponseWriter, r *http.Request) {
	entries, last := l.Ring.Select(l.query(r))
	if entries == nil {
		entries = []logbuf.Entry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "last": last, "tags": l.Ring.Tags()})
}

func (l *Logs) clear(w http.ResponseWriter, r *http.Request) {
	l.Ring.Clear()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (l *Logs) export(w http.ResponseWriter, r *http.Request) {
	q := l.query(r)
	q.Limit = 5000
	entries, _ := l.Ring.Select(q)
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "%s [%s] [%s] %s\n", e.TS, e.Level, e.Tag, e.Msg)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="gateway.log"`)
	_, _ = w.Write([]byte(b.String()))
}

// Register adds the routes.
func (l *Logs) Register(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mux.Handle("GET /api/v2/logs", wrap(http.HandlerFunc(l.list)))
	mux.Handle("DELETE /api/v2/logs", wrap(http.HandlerFunc(l.clear)))
	mux.Handle("GET /api/v2/logs/export", wrap(http.HandlerFunc(l.export)))
}
