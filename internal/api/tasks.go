package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"wb2api/internal/ops"
	"wb2api/internal/pool"
	"wb2api/internal/realm"
)

// CN growth-centre routes: the task snapshot, the full auto-run, cat travel and
// the streak keeper. Everything is operator-triggered; only travel and the
// streak keeper also run from the scheduler.

func (o *Operations) accountBrief(acc *pool.Account) map[string]any {
	rec := acc.Record()
	return map[string]any{"uid": rec.UID, "nickname": rec.Nickname, "realm": rec.Realm, "enterprise": ops.IsEnterprise(rec)}
}

func msgOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// cnTargets resolves the accounts a tasks call applies to. A uid must name an
// existing CN account; without one, every enabled CN account is used.
func (o *Operations) cnTargets(uid string) ([]*pool.Account, string) {
	if uid != "" {
		acc := o.Pool.Get(uid)
		if acc == nil || acc.Record().Realm != realm.CN {
			return nil, "未找到指定的国内版账号"
		}
		return []*pool.Account{acc}, ""
	}
	var out []*pool.Account
	for _, acc := range o.Pool.All() {
		rec := acc.Record()
		if rec.Realm == realm.CN && rec.IsEnabled() && rec.AccessToken != "" {
			out = append(out, acc)
		}
	}
	if len(out) == 0 {
		return nil, "未找到已启用的国内版账号"
	}
	return out, ""
}

func (o *Operations) allCN() []map[string]any {
	out := []map[string]any{}
	for _, acc := range o.Pool.All() {
		rec := acc.Record()
		if rec.Realm == realm.CN && rec.IsEnabled() && rec.AccessToken != "" {
			out = append(out, o.accountBrief(acc))
		}
	}
	return out
}

func (o *Operations) tasksGet(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 60*time.Second)
	defer cancel()
	uid := r.URL.Query().Get("uid")
	var acc *pool.Account
	if uid != "" {
		a := o.Pool.Get(uid)
		if a == nil || a.Record().Realm != realm.CN {
			writeErr(w, http.StatusNotFound, "未找到指定的国内版账号")
			return
		}
		acc = a
	} else {
		for _, a := range o.Pool.All() {
			rec := a.Record()
			if rec.Realm == realm.CN && rec.IsEnabled() && rec.AccessToken != "" {
				acc = a
				break
			}
		}
	}
	if acc == nil {
		writeErr(w, http.StatusNotFound, "未找到已启用的国内版账号")
		return
	}
	snap := o.Ops.GrowthSnapshot(ctx, acc)
	out := map[string]any{"account": o.accountBrief(acc), "accounts": o.allCN()}
	for k, v := range snap {
		out[k] = v
	}
	writeJSON(w, http.StatusOK, out)
}

func (o *Operations) tasksRun(w http.ResponseWriter, r *http.Request) {
	var in struct {
		UID string `json:"uid"`
	}
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	targets, msg := o.cnTargets(in.UID)
	if targets == nil {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Minute)
	defer cancel()
	var logs []string
	credit := 0
	for i, acc := range targets {
		if ctx.Err() != nil {
			break
		}
		accLogs, earned := o.Ops.RunGrowthTasks(ctx, acc)
		logs = append(logs, accLogs...)
		credit += earned
		if i < len(targets)-1 {
			select {
			case <-time.After(1500 * time.Millisecond):
			case <-ctx.Done():
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credit_added": credit, "logs": logs, "accounts_count": len(targets)})
}

func (o *Operations) tasksTravel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		UID string `json:"uid"`
	}
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	targets, msg := o.cnTargets(in.UID)
	if targets == nil {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
	defer cancel()
	results := []map[string]any{}
	var msgs []string
	for i, acc := range targets {
		if ctx.Err() != nil {
			break
		}
		res := o.Ops.CatTravel(ctx, acc)
		item := map[string]any{"uid": acc.Record().UID, "nickname": acc.Record().Nickname, "action": "", "msg": msgOr(res.Msg, res.Error)}
		if res.Extra != nil {
			if a, ok := res.Extra["action"].(string); ok {
				item["action"] = a
			}
			if c, ok := res.Extra["credit"].(int); ok {
				item["reward_credit"] = c
			}
		}
		results = append(results, item)
		msgs = append(msgs, res.Msg)
		if i < len(targets)-1 {
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results, "msg": strings.Join(msgs, "\n"), "accounts_count": len(targets)})
}

func (o *Operations) tasksStreak(w http.ResponseWriter, r *http.Request) {
	var in struct {
		UID string `json:"uid"`
	}
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	targets, msg := o.cnTargets(in.UID)
	if targets == nil {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	defer cancel()
	var msgs []string
	for i, acc := range targets {
		if ctx.Err() != nil {
			break
		}
		res := o.Ops.RunStreakBonus(ctx, acc)
		msgs = append(msgs, res.Msg)
		if i < len(targets)-1 {
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": strings.Join(msgs, "\n"), "accounts_count": len(targets)})
}
