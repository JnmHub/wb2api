package api

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// quotaScanGet lists the active quota scan's watch list and last results.
func (o *Operations) quotaScanGet(w http.ResponseWriter, r *http.Request) {
	if o.Scanner == nil {
		writeErr(w, http.StatusServiceUnavailable, "额度主动查询不可用")
		return
	}
	var models []string
	if o.Scanner.Models != nil {
		models = o.Scanner.Models()
	}
	if models == nil {
		models = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": len(models) > 0, "models": models, "rows": o.Scanner.Snapshot()})
}

// quotaScanRun probes the due lines now (force ignores the schedule window).
func (o *Operations) quotaScanRun(w http.ResponseWriter, r *http.Request) {
	if o.Scanner == nil {
		writeErr(w, http.StatusServiceUnavailable, "额度主动查询不可用")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	sent := o.Scanner.RunDue(ctx, o.Pool, true)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sent": sent, "msg": "已查询 " + strconv.Itoa(sent) + " 条配额线"})
}
