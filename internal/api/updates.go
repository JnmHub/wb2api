package api

import (
	"net/http"

	"wb2api/internal/updates"
)

// Updates serves the read-only update status and the manual check.
type Updates struct{ Checker *updates.Checker }

func (u *Updates) get(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, u.Checker.Status())
}

func (u *Updates) check(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, u.Checker.Check(r.Context(), true))
}

// Register mounts the routes; wrap is the panel authentication.
func (u *Updates) Register(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	if u.Checker == nil {
		return
	}
	h := func(f http.HandlerFunc) http.Handler { return wrap(f) }
	mux.Handle("GET /api/v2/updates", h(u.get))
	mux.Handle("POST /api/v2/updates/check", h(u.check))
}
