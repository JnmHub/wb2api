package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wb2api/internal/activity"
	"wb2api/internal/agents"
	"wb2api/internal/atrest"
	"wb2api/internal/codeartsync"
	"wb2api/internal/gateway/openai"
	"wb2api/internal/jobs"
	"wb2api/internal/ops"
	"wb2api/internal/pool"
	"wb2api/internal/quota"
	"wb2api/internal/raccoonsync"
	"wb2api/internal/realm"
	"wb2api/internal/refresh"
	"wb2api/internal/scheduler"
	"wb2api/internal/store/accounts"
	"wb2api/internal/store/settings"
)

// Prober tests an account with a real request.
type Prober interface {
	Probe(ctx context.Context, acc *pool.Account, model string) openai.ProbeResult
}

// upstreamAction builds the job function of a batch action that talks to the
// vendor.
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
		if len(b.UIDs) == 1 {
			switch {
			case realm.IsCodeArts(acc.Realm) && (b.Action == "checkin" || b.Action == "refresh_credits"):
				ctx = refresh.WithManualCodeArts(ctx, uid)
			case realm.IsRaccoon(acc.Realm) && (b.Action == "checkin" || b.Action == "refresh_credits" || b.Action == "sync_profile"):
				ctx = refresh.WithManualRaccoon(ctx, uid)
			}
		}
		return a.Gate.Do(ctx, func() error {
			switch b.Action {
			case "refresh_credits":
				if realm.IsRaccoon(acc.Record().Realm) {
					if a.Raccoon == nil {
						return errors.New("raccoon channel is not enabled")
					}
					_, err := a.Raccoon.Credits(ctx, acc)
					return err
				}
				if realm.IsCodeArts(acc.Record().Realm) && a.CodeArts != nil {
					_, err := a.CodeArts.Balance(ctx, acc)
					return err
				}
				return resultErr(a.Ops.FetchCredits(ctx, acc))
			case "checkin":
				// The three channels have unrelated check-in APIs: Huawei
				// claims credit campaigns, raccoon triggers the desktop grant
				// and verifies the ledger, WorkBuddy claims its own.
				if realm.IsRaccoon(acc.Record().Realm) {
					if a.Raccoon == nil {
						return errors.New("raccoon channel is not enabled")
					}
					result, err := a.Raccoon.Checkin(ctx, acc)
					if err != nil {
						return err
					}
					if !result.Success {
						return errors.New(orDefault(result.Message, "尚未确认小浣熊签到入账"))
					}
					return nil
				}
				if realm.IsCodeArts(acc.Record().Realm) {
					return a.checkinCodeArts(ctx, acc)
				}
				return resultErr(a.Ops.Checkin(ctx, acc, "manual"))
			case "daily_chat":
				return resultErr(a.Ops.DailyChat(ctx, acc, "manual"))
			case "daily_chat_web":
				return resultErr(a.Ops.DailyChatWeb(ctx, acc))
			case "report_activity":
				return resultErr(a.Ops.ReportActivity(ctx, acc, "manual"))
			case "sync_profile":
				return resultErr(syncAccountProfile(ctx, acc, a.Ops, a.Raccoon))
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

func syncAccountProfile(ctx context.Context, acc *pool.Account, wb *ops.Client, rc *raccoonsync.Service) ops.Result {
	if !realm.IsRaccoon(acc.Realm) {
		return wb.SyncProfile(ctx, acc)
	}
	if rc == nil {
		return ops.Result{Error: "小浣熊渠道未启用"}
	}
	nickname, err := rc.SyncNickname(ctx, acc)
	if err != nil {
		return ops.Result{Error: err.Error()}
	}
	return ops.Result{OK: true, Msg: nickname, Extra: map[string]any{"nickname": nickname}}
}

// Operations serves the single-account actions, login, import / export, the
// activity history and the scheduler.
// Push is the API-key authenticated account push handler, wired from the panel's
// Accounts handler (which owns the settings store).
type Operations struct {
	Ops       *ops.Client
	Pool      *pool.Pool
	Prober    Prober
	Scheduler *scheduler.Scheduler
	Activity  *activity.Log
	Scanner   *quota.Scanner
	AtRest    *atrest.Keyring
	Agents    *agents.Manager
	// Persist stores a changed account record (used by the channel check-in).
	Persist func(*accounts.Record) error
	// CodeArts is the Huawei Cloud channel's operations service (nil = off).
	CodeArts *codeartsync.Service
	// Raccoon is the SenseTime channel's operations service (nil = off).
	Raccoon *raccoonsync.Service
	// CodeArtsLoginBase overrides the snap-access root used by the ticket poll
	// (tests, mock).
	CodeArtsLoginBase string
	// CodeArtsSTSBase overrides the STS root used for the code exchange and the
	// refresh chain (tests, mock).
	CodeArtsSTSBase string

	// Push handles POST /api/v2/accounts/push (nil = disabled).
	Push http.HandlerFunc

	// codeartsLogins holds in-flight CodeArts browser logins.
	codeartsLogins codeartsLogins
	// raccoonLogins holds in-flight raccoon sign-ins (state -> attempt).
	raccoonLogins raccoonLogins
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
	timeout := 3 * time.Minute
	if r.PathValue("name") == "daily-chat-web" {
		timeout = 20 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), timeout)
	defer cancel()
	var res any
	name := r.PathValue("name")
	if realm.IsRaccoon(acc.Realm) && (name == "checkin" || name == "credits" || name == "sync-profile") {
		ctx = refresh.WithManualRaccoon(ctx, acc.UID)
	}
	if realm.IsCodeArts(acc.Realm) && (name == "checkin" || name == "credits") {
		ctx = refresh.WithManualCodeArts(ctx, acc.UID)
	}
	switch name {
	case "credits":
		// The three channels report credits through unrelated endpoints.
		if realm.IsRaccoon(acc.Record().Realm) {
			out := map[string]any{"ok": false, "msg": "", "error": ""}
			if o.Raccoon == nil {
				out["error"] = "小浣熊渠道未启用"
				out["msg"] = out["error"]
				res = out
				break
			}
			usage, err := o.Raccoon.Credits(ctx, acc)
			if err != nil {
				out["error"], out["msg"] = err.Error(), err.Error()
				res = out
				break
			}
			out["ok"] = true
			out["remain"] = usage.Available
			out["wallets"] = usage.Wallets
			out["msg"] = fmt.Sprintf("剩 %d 积分", usage.Available)
			res = out
			break
		}
		// The Huawei channel reports credits through its subscription
		// statistics, not through the WorkBuddy billing API.
		if realm.IsCodeArts(acc.Record().Realm) {
			out := map[string]any{"ok": false, "msg": "", "error": ""}
			if o.CodeArts == nil {
				out["error"] = "CodeArts 渠道未启用"
				out["msg"] = out["error"]
				res = out
				break
			}
			b, err := o.CodeArts.Balance(ctx, acc)
			out["remain"], out["total"] = b.Remaining, b.Total
			out["plan"], out["reset"] = b.PlanName, b.ResetDate
			if err != nil {
				out["error"] = err.Error()
				out["msg"] = err.Error()
				res = out
				break
			}
			out["ok"] = true
			switch {
			case b.HasRemaining && b.HasTotal:
				out["msg"] = fmt.Sprintf("剩 %g / %g 积分（%s，%s 重置）", b.Remaining, b.Total, orDefault(b.PlanName, "套餐"), b.ResetDate)
			case b.HasRemaining:
				out["msg"] = fmt.Sprintf("剩 %g 积分", b.Remaining)
			default:
				out["msg"] = "积分信息已刷新，华为未返回可用余额"
			}
			res = out
			break
		}
		res = o.Ops.FetchCredits(ctx, acc)
	case "checkin":
		// The three channels have unrelated check-in APIs: Huawei claims credit
		// campaigns, raccoon triggers the desktop "home" endpoint and verifies
		// it against the bill ledger, WorkBuddy claims its own.
		if realm.IsRaccoon(acc.Record().Realm) {
			out := map[string]any{"ok": false, "msg": "", "error": ""}
			if o.Raccoon == nil {
				out["error"] = "小浣熊渠道未启用"
				out["msg"] = out["error"]
				res = out
				break
			}
			result, err := o.Raccoon.Checkin(ctx, acc)
			out["ok"] = result.Success && err == nil
			out["msg"] = result.Message
			out["credits"] = result.Credit()
			out["already_completed"] = result.AlreadyCompleted
			if err != nil {
				out["error"] = err.Error()
				if msg, _ := out["msg"].(string); msg == "" {
					out["msg"] = err.Error()
				}
			}
			res = out
			break
		}
		if realm.IsCodeArts(acc.Record().Realm) {
			out := map[string]any{"ok": false, "msg": "", "error": ""}
			if o.CodeArts == nil {
				out["error"] = "CodeArts 渠道未启用"
				out["msg"] = out["error"]
				res = out
				break
			}
			ok, msg, err := o.CodeArts.Checkin(ctx, acc, true)
			out["ok"] = ok && err == nil
			out["msg"] = msg
			if err != nil {
				out["error"] = err.Error()
				if msg == "" {
					out["msg"] = err.Error()
				}
			}
			res = out
			break
		}
		res = o.Ops.Checkin(ctx, acc, "manual")
	case "claim":
		// One-off sign-up grants. Only the raccoon channel has them (WorkBuddy
		// has no claim surface, CodeArts claims during discovery).
		if !realm.IsRaccoon(acc.Record().Realm) {
			writeErr(w, http.StatusNotFound, "unknown action claim")
			return
		}
		out := map[string]any{"ok": false, "msg": "", "error": ""}
		if o.Raccoon == nil {
			out["error"] = "小浣熊渠道未启用"
			out["msg"] = out["error"]
			res = out
			break
		}
		result, err := o.Raccoon.Claim(ctx, acc)
		out["ok"] = err == nil
		out["earned"] = result.Earned
		out["tasks"] = result.Outcomes
		out["msg"] = fmt.Sprintf("领取 %d 积分", result.Earned)
		if len(result.Outcomes) == 0 {
			out["msg"] = "新手任务已全部领取，无需重复请求"
		}
		if err != nil {
			out["error"], out["msg"] = err.Error(), err.Error()
		}
		res = out
	case "daily-chat":
		res = o.Ops.DailyChat(ctx, acc, "manual")
	case "daily-chat-web":
		res = o.Ops.DailyChatWeb(ctx, acc)
	case "report-activity":
		res = o.Ops.ReportActivity(ctx, acc, "manual")
	case "sync-profile":
		res = syncAccountProfile(ctx, acc, o.Ops, o.Raccoon)
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
	if realm.IsCodeArts(in.Realm) {
		res, err := o.startCodeArtsLogin(r)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, res)
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
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if _, ok := o.codeartsLogins.get(state); ok {
		writeJSON(w, http.StatusOK, o.pollCodeArtsLogin(context.WithoutCancel(r.Context()), state))
		return
	}
	writeJSON(w, http.StatusOK, o.Ops.LoginPoll(r.Context(), state))
}

func (o *Operations) loginCancel(w http.ResponseWriter, r *http.Request) {
	var in struct{ State string }
	if !decode(w, r, &in) {
		return
	}
	if _, ok := o.codeartsLogins.get(in.State); ok {
		o.codeartsLogins.drop(in.State)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
	if !decodeN(w, r, &in, 32<<20) {
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
	in.Realm = strings.ToLower(strings.TrimSpace(in.Realm))
	if in.Realm != "" && !realm.Valid(in.Realm) {
		writeErr(w, http.StatusBadRequest, "realm must be intl, cn, codearts or raccoon")
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
	case "", "ok", "failed", codeartsync.DailyClaimed, codeartsync.DailyAlready,
		codeartsync.DailyUnavailable, codeartsync.DailyWaiting, codeartsync.DailyPending,
		codeartsync.DailyNeedsLogin, codeartsync.DailyExhausted:
	default:
		writeErr(w, http.StatusBadRequest, "unknown activity result")
		return
	}
	writeJSON(w, http.StatusOK, o.Activity.Query(f))
}

func (o *Operations) schedulerGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, o.Scheduler.Status())
}

func (o *Operations) schedulerPatch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled  *bool                        `json:"enabled"`
		Schedule map[string]any               `json:"schedule"`
		CodeArts *settings.CodeArtsDailyPatch `json:"codearts_daily"`
		Raccoon  *settings.RaccoonDailyPatch  `json:"raccoon_daily"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Raccoon != nil {
		if err := in.Raccoon.Validate(); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := o.Scheduler.Settings.SetRaccoonDaily(*in.Raccoon); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if in.CodeArts != nil {
		if err := in.CodeArts.Validate(); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := o.Scheduler.Settings.SetCodeArtsDaily(*in.CodeArts); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
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
		writeErr(w, http.StatusConflict, "已有巡检正在执行，请稍候再试")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "msg": "已触发后台调度执行"})
}

func (o *Operations) schedulerTriggerCodeArts(w http.ResponseWriter, r *http.Request) {
	if o.Scheduler.Channel == nil {
		writeErr(w, http.StatusServiceUnavailable, "CodeArts 签到未启用")
		return
	}
	if !o.Scheduler.TriggerCodeArts() {
		writeErr(w, http.StatusConflict, "已有 CodeArts 签到任务正在执行")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "msg": "已触发 CodeArts 签到"})
}

// schedulerTriggerRaccoon runs the raccoon daily task now.
func (o *Operations) schedulerTriggerRaccoon(w http.ResponseWriter, r *http.Request) {
	if o.Scheduler == nil || o.Scheduler.Raccoon == nil {
		writeErr(w, http.StatusServiceUnavailable, "小浣熊签到未启用")
		return
	}
	if !o.Scheduler.TriggerRaccoon() {
		writeErr(w, http.StatusConflict, "已有小浣熊签到任务正在执行，或没有可签到的账号")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "msg": "已触发小浣熊签到"})
}

// Register mounts the endpoints; wrap is the panel authentication.
func (o *Operations) Register(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	h := func(f http.HandlerFunc) http.Handler { return wrap(f) }
	mux.Handle("POST /api/v2/accounts/{uid}/actions/{name}", h(o.action))
	mux.Handle("POST /api/v2/accounts/login/start", h(o.loginStart))
	mux.Handle("GET /api/v2/accounts/login/poll", h(o.loginPoll))
	mux.Handle("POST /api/v2/accounts/login/cancel", h(o.loginCancel))
	mux.Handle("POST /api/v2/codearts/models/refresh", h(o.codeartsModelsRefresh))
	// Raccoon (小浣熊): sign-in, callback landing and catalogue refresh.
	mux.Handle("POST /api/v2/raccoon/login/start", h(o.raccoonLoginStart))
	mux.Handle("GET /api/v2/raccoon/login/poll", h(o.raccoonLoginPoll))
	mux.Handle("POST /api/v2/raccoon/login/complete", h(o.raccoonLoginComplete))
	mux.Handle("POST /api/v2/raccoon/login/cancel", h(o.raccoonLoginCancel))
	// The authorize page navigates the browser here; no panel session is
	// available on that request, so the state parameter is the only credential.
	mux.HandleFunc("GET /api/v2/raccoon/login/callback", o.raccoonLoginCallback)
	mux.Handle("POST /api/v2/raccoon/models/refresh", h(o.raccoonModelsRefresh))
	// Pasting the official client's auth.json (or a bare token) is the fallback
	// that works from any deployment.
	mux.Handle("POST /api/v2/raccoon/accounts/import", h(o.raccoonImportAccount))
	// Remote deployments cannot reach the loopback callback; the operator pastes
	// the address bar back into the panel instead.
	mux.Handle("POST /api/v2/codearts/login/complete", h(o.codeartsLoginComplete))
	// The portal calls this back from the browser, without the panel session:
	// it must stay unauthenticated.
	mux.HandleFunc("/oauth/callback", o.codeartsCallback)
	mux.Handle("POST /api/v2/accounts/import", h(o.importAccounts))
	// Automation pushes accounts with an API key: no panel session, no CSRF.
	if o.Push != nil {
		mux.HandleFunc("POST /api/v2/accounts/push", o.Push)
	}
	mux.Handle("GET /api/v2/accounts/export", h(o.exportAccounts))
	mux.Handle("GET /api/v2/activity", h(o.activityList))
	mux.Handle("GET /api/v2/scheduler", h(o.schedulerGet))
	mux.Handle("PATCH /api/v2/scheduler", h(o.schedulerPatch))
	mux.Handle("POST /api/v2/scheduler/trigger", h(o.schedulerTrigger))
	mux.Handle("POST /api/v2/scheduler/trigger-codearts", h(o.schedulerTriggerCodeArts))
	mux.Handle("POST /api/v2/scheduler/trigger-raccoon", h(o.schedulerTriggerRaccoon))
	mux.Handle("GET /api/v2/tasks", h(o.tasksGet))
	mux.Handle("POST /api/v2/tasks/run", h(o.tasksRun))
	mux.Handle("POST /api/v2/tasks/travel", h(o.tasksTravel))
	mux.Handle("POST /api/v2/tasks/streak-bonus", h(o.tasksStreak))
	mux.Handle("GET /api/v2/quota-scan", h(o.quotaScanGet))
	mux.Handle("POST /api/v2/quota-scan/run", h(o.quotaScanRun))
	mux.Handle("GET /api/v2/accounts/desktop", h(o.desktopGet))
	mux.Handle("POST /api/v2/accounts/desktop", h(o.desktopPost))
	mux.Handle("GET /api/v2/agents", h(o.agentsGet))
	mux.Handle("POST /api/v2/agents/apply", h(o.agentsApply))
	mux.Handle("POST /api/v2/agents/restore", h(o.agentsRestore))
}
