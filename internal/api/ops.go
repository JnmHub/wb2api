package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wb2api/internal/activity"
	"wb2api/internal/gateway/openai"
	"wb2api/internal/jobs"
	"wb2api/internal/ops"
	"wb2api/internal/pool"
	"wb2api/internal/scheduler"
)

// Prober tests an account with a real request.
type Prober interface {
	Probe(ctx context.Context, acc *pool.Account, model string) openai.ProbeResult
}

// upstreamSlots bounds how many vendor-facing account calls run at once: each
// one is a real request, so a big batch must not become a burst.
var upstreamSlots = make(chan struct{}, 4)

func withSlot(ctx context.Context, f func() error) error {
	select {
	case upstreamSlots <- struct{}{}:
		defer func() { <-upstreamSlots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	return f()
}

func resultErr(r ops.Result) error {
	if r.OK {
		return nil
	}
	if r.Error != "" {
		return errors.New(r.Error)
	}
	return errors.New(r.Msg)
}

// upstreamAction builds the job function of a batch action that talks to the
// vendor.
func (a *Accounts) upstreamAction(b batchReq) (jobs.Func, error) {
	if a.Ops == nil {
		return nil, errors.New("account operations are not available")
	}
	model, _ := b.Params["model"].(string)
	return func(ctx context.Context, uid string) error {
		acc := a.Pool.Get(uid)
		if acc == nil {
			return errors.New("account no longer exists")
		}
		return withSlot(ctx, func() error {
			switch b.Action {
			case "refresh_credits":
				return resultErr(a.Ops.FetchCredits(ctx, acc))
			case "checkin":
				return resultErr(a.Ops.Checkin(ctx, acc, "manual"))
			case "daily_chat":
				return resultErr(a.Ops.DailyChat(ctx, acc, "manual"))
			case "report_activity":
				return resultErr(a.Ops.ReportActivity(ctx, acc, "manual"))
			case "sync_profile":
				return resultErr(a.Ops.SyncProfile(ctx, acc))
			case "test":
				if a.Prober == nil {
					return errors.New("the account test is not available")
				}
				r := a.Prober.Probe(ctx, acc, model)
				if !r.OK {
					return errors.New(r.Error)
				}
			}
			return nil
		})
	}, nil
}

// Operations serves the single-account actions, login, import / export, the
// activity history and the scheduler.
type Operations struct {
	Ops       *ops.Client
	Pool      *pool.Pool
	Prober    Prober
	Scheduler *scheduler.Scheduler
	Activity  *activity.Log
}

func (o *Operations) account(w http.ResponseWriter, r *http.Request) *pool.Account {
	acc := o.Pool.Get(r.PathValue("uid"))
	if acc == nil {
		writeErr(w, http.StatusNotFound, "no such account")
	}
	return acc
}

func (o *Operations) action(w http.ResponseWriter, r *http.Request) {
	acc := o.account(w, r)
	if acc == nil {
		return
	}
	var in struct {
		Model string `json:"model"`
	}
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Minute)
	defer cancel()
	var res any
	switch name := r.PathValue("name"); name {
	case "credits":
		res = o.Ops.FetchCredits(ctx, acc)
	case "checkin":
		res = o.Ops.Checkin(ctx, acc, "manual")
	case "daily-chat":
		res = o.Ops.DailyChat(ctx, acc, "manual")
	case "report-activity":
		res = o.Ops.ReportActivity(ctx, acc, "manual")
	case "sync-profile":
		res = o.Ops.SyncProfile(ctx, acc)
	case "test":
		if o.Prober == nil {
			writeErr(w, http.StatusNotImplemented, "the account test is not available")
			return
		}
		res = o.Prober.Probe(ctx, acc, in.Model)
	default:
		writeErr(w, http.StatusNotFound, "unknown action "+name)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (o *Operations) loginStart(w http.ResponseWriter, r *http.Request) {
	var in struct{ Realm, Platform string }
	if !decode(w, r, &in) {
		return
	}
	res, err := o.Ops.LoginStart(r.Context(), in.Realm, in.Platform)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (o *Operations) loginPoll(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, o.Ops.LoginPoll(r.Context(), r.URL.Query().Get("state")))
}

func (o *Operations) loginCancel(w http.ResponseWriter, r *http.Request) {
	var in struct{ State string }
	if !decode(w, r, &in) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": o.Ops.CancelLogin(in.State)})
}

// import accepts the document either as a JSON string (pasted text) or inline.
func (o *Operations) importAccounts(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Document  any    `json:"document"`
		Realm     string `json:"realm"`
		Overwrite bool   `json:"overwrite"`
		DryRun    bool   `json:"dry_run"`
	}
	if !decode(w, r, &in) {
		return
	}
	var raw []byte
	switch d := in.Document.(type) {
	case string:
		raw = []byte(d)
	case nil:
		writeErr(w, http.StatusBadRequest, "document is required")
		return
	default:
		b, _ := jsonMarshal(d)
		raw = b
	}
	if in.Realm != "" && in.Realm != "intl" && in.Realm != "cn" {
		writeErr(w, http.StatusBadRequest, "realm must be intl or cn")
		return
	}
	rows, err := ops.ParseDocument(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(rows) > 5000 {
		writeErr(w, http.StatusBadRequest, "at most 5000 accounts per import")
		return
	}
	rep := o.Ops.Import(rows, in.Realm, in.Overwrite, in.DryRun)
	writeJSON(w, http.StatusOK, map[string]any{"count": len(rows), "dry_run": in.DryRun, "result": rep})
}

func (o *Operations) exportAccounts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var uids []string
	if u := q.Get("uids"); u != "" {
		uids = strings.Split(u, ",")
	}
	doc, err := o.Ops.Export(q.Get("realm"), uids, q.Get("secrets") != "0")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	b, _ := jsonIndent(doc)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="workbuddy-accounts.json"`)
	_, _ = w.Write(b)
}

func (o *Operations) activityList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	f := activity.Filter{Range: q.Get("range"), UID: q.Get("uid"), Task: q.Get("task"), Result: q.Get("result"), Limit: limit}
	switch f.Result {
	case "", "ok", "failed":
	default:
		writeErr(w, http.StatusBadRequest, "result must be ok or failed")
		return
	}
	writeJSON(w, http.StatusOK, o.Activity.Query(f))
}

func (o *Operations) schedulerGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, o.Scheduler.Status())
}

func (o *Operations) schedulerPatch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled  *bool          `json:"enabled"`
		Schedule map[string]any `json:"schedule"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Schedule != nil {
		if _, err := o.Scheduler.Settings.SetScheduleHours(in.Schedule); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if in.Enabled != nil {
		if err := o.Scheduler.Settings.SetSchedulerEnabled(*in.Enabled); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, o.Scheduler.Status())
}

func (o *Operations) schedulerTrigger(w http.ResponseWriter, r *http.Request) {
	if !o.Scheduler.Trigger() {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "msg": "已有巡检正在执行，请稍候再试"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "msg": "已触发后台调度执行"})
}

// Register mounts the endpoints; wrap is the panel authentication.
func (o *Operations) Register(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	h := func(f http.HandlerFunc) http.Handler { return wrap(f) }
	mux.Handle("POST /api/v2/accounts/{uid}/actions/{name}", h(o.action))
	mux.Handle("POST /api/v2/accounts/login/start", h(o.loginStart))
	mux.Handle("GET /api/v2/accounts/login/poll", h(o.loginPoll))
	mux.Handle("POST /api/v2/accounts/login/cancel", h(o.loginCancel))
	mux.Handle("POST /api/v2/accounts/import", h(o.importAccounts))
	mux.Handle("GET /api/v2/accounts/export", h(o.exportAccounts))
	mux.Handle("GET /api/v2/activity", h(o.activityList))
	mux.Handle("GET /api/v2/scheduler", h(o.schedulerGet))
	mux.Handle("PATCH /api/v2/scheduler", h(o.schedulerPatch))
	mux.Handle("POST /api/v2/scheduler/trigger", h(o.schedulerTrigger))
}
