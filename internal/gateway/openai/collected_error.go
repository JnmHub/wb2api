package openai

import (
	"net/http"
)

func markCollectedError(ev *UsageEvent, r *http.Request) {
	if r.Context().Err() != nil {
		ev.Outcome, ev.Status = "client_aborted", http.StatusOK
		return
	}
	ev.Outcome, ev.Status = "upstream_aborted", http.StatusBadGateway
}
