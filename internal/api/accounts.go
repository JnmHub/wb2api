package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"wb2api/internal/events"
	"wb2api/internal/fingerprint"
	"wb2api/internal/jobs"
	"wb2api/internal/ops"
	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/refresh"
	"wb2api/internal/store/accounts"
	"wb2api/internal/store/settings"
)

// TokenRefresher is what the accounts API needs from the refresher.
type TokenRefresher interface {
	Refresh(ctx context.Context, a *pool.Account) error
	StatusOf(uid string) refresh.Status
}

// Accounts serves the account-management endpoints. Lists are paged, filtered
// and sorted on the server, so the browser only ever holds one page of rows.
type Accounts struct {
	Pool      *pool.Pool
	Today     *Today
	Store     *settings.Store
	Persist   func(*accounts.Record) error
	Delete    func(*accounts.Record) error
	Refresher TokenRefresher
	Jobs      *jobs.Manager
	Bus       *events.Bus
	Now       func() time.Time
	// Ops runs the vendor-facing account operations (balances, check-in, ...);
	// Prober tests an account with a real request. Either may be nil.
	Ops    *ops.Client
	Prober Prober

	changes events.Coalescer
}

func (a *Accounts) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// ------------------------------------------------------------------- views

func (a *Accounts) view(acc *pool.Account, now time.Time, detail bool) map[string]any {
	rec := acc.Record()
	today := a.Today.Get(acc.UID)
	status := acc.State(now)
	nick := rec.Nickname
	if nick == "" {
		nick = shortID(rec.UID)
	}
	v := map[string]any{
		"uid": rec.UID, "short_id": shortID(rec.UID), "nickname": nick, "realm": rec.Realm, "product": rec.Product,
		"enterprise_id": rec.EnterpriseID, "enabled": acc.Enabled(), "weight": acc.Weight(), "note": rec.Note,
		"disabled_models": rec.DisabledModels, "source": rec.Source, "proxy_slot": rec.ProxySlot, "proxy": rec.ProxyLegacy,
		"expires_at": rec.ExpiresAt, "status": string(status), "need_login": status == pool.StatusNeedLogin,
		"has_refresh_token": rec.RefreshToken != "", "added_at": rec.AddedAt, "inflight": acc.Inflight(),
		"today": map[string]any{"requests": today.Requests, "tokens": today.Tokens, "credit": today.Credit},
	}
	if until := acc.CooldownUntil(); until.After(now) {
		v["cooldown_for"] = int(until.Sub(now).Seconds()) + 1
	}
	if until := acc.RestrictedUntil(); until.After(now) {
		v["restricted_for"] = int(until.Sub(now).Seconds()) + 1
	}
	if mc := acc.ModelCooldowns(now); len(mc) > 0 {
		list := make([]map[string]any, 0, len(mc))
		for _, m := range mc {
			list = append(list, map[string]any{"model": m.Model, "expires_at": m.Until.Unix()})
		}
		v["model_cooldowns"] = list
	}
	if rec.Credits != nil {
		v["credits"] = summarizeCredits(rec.Credits)
	}
	if a.Refresher != nil {
		if st := a.Refresher.StatusOf(rec.UID); st.Failures > 0 {
			v["refresh"] = map[string]any{"failures": st.Failures, "next_try": st.NextTry.Unix(), "last_error": st.LastErr}
		}
	}
	if detail {
		v["machine_id"] = fingerprint.MachineID(rec.UID)
		v["session_id"] = fingerprint.SessionID(rec.UID)
		v["domain"] = rec.Domain
		v["platform"] = rec.Platform
		v["restricted_until"] = rec.RestrictedUntil
		v["last_checkin"], v["last_daily_chat"], v["last_activity_report"] = rec.LastCheckin, rec.LastDailyChat, rec.LastActivityReport
		v["model_caps"] = rec.ModelCaps
		if rec.Credits != nil {
			v["credits_raw"] = rec.Credits
		}
		st := acc.Snapshot()
		v["stats"] = map[string]any{"requests": st.Requests, "failures": st.Failures}
	}
	return v
}

func shortID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// summarizeCredits keeps the figures a list needs (remaining / total) out of
// the raw billing document.
func summarizeCredits(c map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"remain", "total", "used", "updatedAt", "updated_at"} {
		if v, ok := c[k]; ok {
			out[k] = v
		}
	}
	return out
}

func numberOf(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case float64:
		return t, true
	}
	return 0, false
}

// -------------------------------------------------------------- list / filter

type query struct {
	realm   string
	status  map[pool.Status]bool
	q       string
	enabled *bool
	sortBy  string
	desc    bool
	offset  int
	limit   int
}

func parseQuery(r *http.Request) (query, error) {
	v := r.URL.Query()
	q := query{realm: v.Get("realm"), q: strings.ToLower(strings.TrimSpace(v.Get("q"))), sortBy: v.Get("sort"), limit: 100}
	if q.realm == "all" {
		q.realm = ""
	}
	if s := v.Get("status"); s != "" && s != "all" {
		q.status = map[pool.Status]bool{}
		for _, p := range strings.Split(s, ",") {
			p = strings.TrimSpace(p)
			switch pool.Status(p) {
			case pool.StatusDisabled, pool.StatusNeedLogin, pool.StatusRestricted, pool.StatusCooling, pool.StatusExpiring, pool.StatusNoCredit, pool.StatusReady:
				q.status[pool.Status(p)] = true
			default:
				return q, fmt.Errorf("unknown status %q", p)
			}
		}
	}
	if s := v.Get("enabled"); s != "" {
		b := s == "true" || s == "1"
		q.enabled = &b
	}
	q.desc = v.Get("order") == "desc"
	if q.sortBy == "" {
		q.sortBy = "default"
	}
	switch q.sortBy {
	case "default", "weight", "name", "added", "expires", "tokens", "requests", "status", "credits":
	default:
		return q, fmt.Errorf("cannot sort by %q", q.sortBy)
	}
	if n, err := strconv.Atoi(v.Get("offset")); err == nil && n > 0 {
		q.offset = n
	}
	if n, err := strconv.Atoi(v.Get("limit")); err == nil && n > 0 {
		q.limit = n
	}
	if q.limit > 500 {
		q.limit = 500
	}
	return q, nil
}

type row struct {
	acc    *pool.Account
	status pool.Status
	name   string
	day    DayStat
	credit float64
	hasCr  bool
	idx    int
}

var statusRank = map[pool.Status]int{
	pool.StatusReady: 0, pool.StatusExpiring: 1, pool.StatusCooling: 2, pool.StatusRestricted: 3,
	pool.StatusNeedLogin: 4, pool.StatusDisabled: 5,
}

// matching returns the rows passing q, ordered.
func (a *Accounts) matching(q query, now time.Time) []row {
	all := a.Pool.All()
	rows := make([]row, 0, len(all))
	for i, acc := range all {
		rec := acc.Record()
		if q.realm != "" && acc.Realm != q.realm {
			continue
		}
		st := acc.State(now)
		if q.status != nil && !q.status[st] {
			continue
		}
		if q.enabled != nil && acc.Enabled() != *q.enabled {
			continue
		}
		name := rec.Nickname
		if name == "" {
			name = rec.UID
		}
		if q.q != "" {
			hit := strings.Contains(strings.ToLower(rec.UID), q.q) || strings.Contains(strings.ToLower(rec.Nickname), q.q) ||
				strings.Contains(strings.ToLower(rec.Note), q.q) || strings.Contains(strings.ToLower(rec.ProxySlot), q.q)
			if !hit {
				continue
			}
		}
		r := row{acc: acc, status: st, name: strings.ToLower(name), idx: i}
		if q.sortBy == "tokens" || q.sortBy == "requests" {
			r.day = a.Today.Get(rec.UID)
		}
		if q.sortBy == "credits" && rec.Credits != nil {
			r.credit, r.hasCr = numberOf(rec.Credits["remain"])
		}
		rows = append(rows, r)
	}
	less := func(i, j int) bool { return rows[i].idx < rows[j].idx }
	switch q.sortBy {
	case "default":
		// Disabled accounts sink; then heavier weights first; stable file order.
		less = func(i, j int) bool {
			di, dj := rows[i].status == pool.StatusDisabled, rows[j].status == pool.StatusDisabled
			if di != dj {
				return !di
			}
			if wi, wj := rows[i].acc.Weight(), rows[j].acc.Weight(); wi != wj {
				return wi > wj
			}
			return rows[i].idx < rows[j].idx
		}
	case "weight":
		less = func(i, j int) bool {
			return cmpInt(rows[i].acc.Weight(), rows[j].acc.Weight(), q.desc, rows[i].idx < rows[j].idx)
		}
	case "name":
		less = func(i, j int) bool { return cmpStr(rows[i].name, rows[j].name, q.desc, rows[i].idx < rows[j].idx) }
	case "added":
		less = func(i, j int) bool {
			return cmpF(rows[i].acc.Record().AddedAt, rows[j].acc.Record().AddedAt, q.desc, rows[i].idx < rows[j].idx)
		}
	case "expires":
		less = func(i, j int) bool {
			return cmpF(float64(rows[i].acc.ExpiresAt()), float64(rows[j].acc.ExpiresAt()), q.desc, rows[i].idx < rows[j].idx)
		}
	case "tokens":
		less = func(i, j int) bool {
			return cmpF(float64(rows[i].day.Tokens), float64(rows[j].day.Tokens), q.desc, rows[i].idx < rows[j].idx)
		}
	case "requests":
		less = func(i, j int) bool {
			return cmpF(float64(rows[i].day.Requests), float64(rows[j].day.Requests), q.desc, rows[i].idx < rows[j].idx)
		}
	case "status":
		less = func(i, j int) bool {
			return cmpInt(statusRank[rows[i].status], statusRank[rows[j].status], q.desc, rows[i].idx < rows[j].idx)
		}
	case "credits":
		less = func(i, j int) bool {
			if rows[i].hasCr != rows[j].hasCr {
				return rows[i].hasCr // unknown balances last
			}
			return cmpF(rows[i].credit, rows[j].credit, q.desc, rows[i].idx < rows[j].idx)
		}
	}
	sort.SliceStable(rows, less)
	return rows
}

func cmpInt(a, b int, desc, idxLess bool) bool {
	if a == b {
		return idxLess
	}
	return (a < b) != desc
}
func cmpF(a, b float64, desc, idxLess bool) bool {
	if a == b {
		return idxLess
	}
	return (a < b) != desc
}
func cmpStr(a, b string, desc, idxLess bool) bool {
	if a == b {
		return idxLess
	}
	return (a < b) != desc
}

func (a *Accounts) list(w http.ResponseWriter, r *http.Request) {
	q, err := parseQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	now := a.now()
	rows := a.matching(q, now)
	total := len(rows)
	if q.offset > total {
		q.offset = total
	}
	end := q.offset + q.limit
	if end > total {
		end = total
	}
	items := make([]map[string]any, 0, end-q.offset)
	for _, rw := range rows[q.offset:end] {
		items = append(items, a.view(rw.acc, now, false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total, "offset": q.offset, "limit": q.limit})
}

func (a *Accounts) summary(w http.ResponseWriter, r *http.Request) {
	now := a.now()
	byStatus := map[string]int{}
	byRealm := map[string]map[string]int{}
	var tokens, requests int64
	var todayCredit float64
	credits := map[string]any{"remain": 0.0, "available": 0.0, "expiring_7d": 0.0, "today_used": 0.0, "known": 0, "zero": 0}
	realmCredits := map[string]map[string]any{}
	creditOf := func(realm string) map[string]any {
		if realmCredits[realm] == nil {
			realmCredits[realm] = map[string]any{"remain": 0.0, "available": 0.0, "expiring_7d": 0.0, "total": 0, "ready": 0, "zero": 0, "keys": 0}
		}
		return realmCredits[realm]
	}
	for _, acc := range a.Pool.All() {
		st := string(acc.State(now))
		byStatus[st]++
		if byRealm[acc.Realm] == nil {
			byRealm[acc.Realm] = map[string]int{}
		}
		byRealm[acc.Realm][st]++
		d := a.Today.Get(acc.UID)
		tokens += d.Tokens
		requests += d.Requests
		todayCredit += d.Credit

		rc := creditOf(acc.Realm)
		rc["total"] = rc["total"].(int) + 1
		if st == string(pool.StatusReady) {
			rc["ready"] = rc["ready"].(int) + 1
		}
		if remain, known := acc.CreditRemain(); known {
			credits["known"] = credits["known"].(int) + 1
			credits["remain"] = credits["remain"].(float64) + remain
			rc["remain"] = rc["remain"].(float64) + remain
			if st == string(pool.StatusReady) {
				credits["available"] = credits["available"].(float64) + remain
				rc["available"] = rc["available"].(float64) + remain
			}
			if remain <= 0 {
				credits["zero"] = credits["zero"].(int) + 1
				rc["zero"] = rc["zero"].(int) + 1
			}
			if exp := acc.CreditExpiringWithin(7); exp > 0 {
				credits["expiring_7d"] = credits["expiring_7d"].(float64) + exp
				rc["expiring_7d"] = rc["expiring_7d"].(float64) + exp
			}
		}
	}
	credits["today_used"] = todayCredit
	keys := 0
	if a.Store != nil {
		for _, k := range a.Store.APIKeys(false) {
			keys++
			if k.Realm != "" {
				creditOf(k.Realm)["keys"] = creditOf(k.Realm)["keys"].(int) + 1
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total": a.Pool.Len(), "by_status": byStatus, "by_realm": byRealm,
		"today":   map[string]any{"tokens": tokens, "requests": requests, "credit": todayCredit},
		"credits": credits, "by_realm_credits": realmCredits, "keys": keys,
	})
}

func (a *Accounts) get(w http.ResponseWriter, r *http.Request) {
	acc := a.Pool.Get(r.PathValue("uid"))
	if acc == nil {
		writeErr(w, http.StatusNotFound, "no such account")
		return
	}
	writeJSON(w, http.StatusOK, a.view(acc, a.now(), true))
}

// ----------------------------------------------------------------- mutations

type patch struct {
	Enabled        *bool     `json:"enabled"`
	Weight         *int      `json:"weight"`
	Note           *string   `json:"note"`
	DisabledModels *[]string `json:"disabled_models"`
	ProxySlot      *string   `json:"proxy_slot"`
	Proxy          *string   `json:"proxy"`
	Product        *string   `json:"product"`
}

func (p patch) validate() error {
	if p.Weight != nil && (*p.Weight < 1 || *p.Weight > 100) {
		return errors.New("weight must be between 1 and 100")
	}
	if p.Note != nil && len([]rune(*p.Note)) > 200 {
		return errors.New("note is limited to 200 characters")
	}
	if p.Product != nil {
		switch strings.ToLower(strings.TrimSpace(*p.Product)) {
		case "workbuddy", "vscode", "cli", "wb", "desktop", "vsc", "ide", "codebuddy":
		default:
			return errors.New("product must be workbuddy, vscode or cli")
		}
	}
	return nil
}

func (p patch) apply(r *accounts.Record) {
	if p.Enabled != nil {
		r.Enabled = *p.Enabled
	}
	if p.Weight != nil {
		r.Weight = *p.Weight
	}
	if p.Note != nil {
		r.Note = strings.TrimSpace(*p.Note)
	}
	if p.DisabledModels != nil {
		list := make([]any, len(*p.DisabledModels))
		for i, m := range *p.DisabledModels {
			list[i] = m
		}
		r.DisabledModels = normaliseModels(list)
	}
	if p.ProxySlot != nil {
		r.ProxySlot = strings.TrimSpace(*p.ProxySlot)
	}
	if p.Proxy != nil {
		r.ProxyLegacy = strings.TrimSpace(*p.Proxy)
	}
	if p.Product != nil {
		r.Product = accounts.NormalizeProduct(*p.Product)
	}
}

func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "cannot read request body")
		return false
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func (a *Accounts) save(rec *accounts.Record) error {
	if a.Persist == nil {
		return nil
	}
	return a.Persist(rec)
}

func (a *Accounts) patch(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	var p patch
	if !readBody(w, r, &p) {
		return
	}
	if err := p.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	rec, ok := a.Pool.Update(uid, p.apply)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such account")
		return
	}
	if err := a.save(rec); err != nil {
		writeErr(w, http.StatusInternalServerError, "changed in memory but could not be saved: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.view(a.Pool.Get(uid), a.now(), true))
}

func (a *Accounts) remove(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	acc := a.Pool.Get(uid)
	if acc == nil {
		writeErr(w, http.StatusNotFound, "no such account")
		return
	}
	rec := acc.Record()
	if !a.Pool.Remove(uid) {
		writeErr(w, http.StatusNotFound, "no such account")
		return
	}
	if a.Delete != nil {
		if err := a.Delete(rec); err != nil {
			writeErr(w, http.StatusInternalServerError, "removed from the pool but the file could not be deleted: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ------------------------------------------------------------------- batches

type batchReq struct {
	Action  string         `json:"action"`
	UIDs    []string       `json:"uids"`
	Filter  *batchFilter   `json:"filter"`
	Params  map[string]any `json:"params"`
	Confirm bool           `json:"confirm"`
}

type batchFilter struct {
	Realm   string `json:"realm"`
	Status  string `json:"status"`
	Q       string `json:"q"`
	Enabled *bool  `json:"enabled"`
}

const maxBatch = 20000

var destructive = map[string]bool{"delete": true, "clear_restriction": true}

var notYet = map[string]string{}

func (a *Accounts) resolveTargets(b batchReq, now time.Time) ([]string, error) {
	if len(b.UIDs) > 0 && b.Filter != nil {
		return nil, errors.New("give either uids or filter, not both")
	}
	if len(b.UIDs) > 0 {
		seen := map[string]bool{}
		var out []string
		for _, u := range b.UIDs {
			if !seen[u] && a.Pool.Get(u) != nil {
				seen[u] = true
				out = append(out, u)
			}
		}
		return out, nil
	}
	if b.Filter == nil {
		return nil, errors.New("give uids or a filter")
	}
	q := query{realm: b.Filter.Realm, q: strings.ToLower(strings.TrimSpace(b.Filter.Q)), enabled: b.Filter.Enabled, sortBy: "default"}
	if q.realm == "all" {
		q.realm = ""
	}
	if b.Filter.Status != "" && b.Filter.Status != "all" {
		q.status = map[pool.Status]bool{}
		for _, s := range strings.Split(b.Filter.Status, ",") {
			q.status[pool.Status(strings.TrimSpace(s))] = true
		}
	}
	rows := a.matching(q, now)
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.acc.UID
	}
	return out, nil
}

func intParam(p map[string]any, key string) (int, bool) {
	f, ok := numberOf(p[key])
	return int(f), ok
}

func (a *Accounts) batchFn(b batchReq) (jobs.Func, error) {
	update := func(mut func(*accounts.Record)) jobs.Func {
		return func(ctx context.Context, uid string) error {
			rec, ok := a.Pool.Update(uid, mut)
			if !ok {
				return errors.New("account no longer exists")
			}
			return a.save(rec)
		}
	}
	switch b.Action {
	case "enable":
		return update(func(r *accounts.Record) { r.Enabled = true }), nil
	case "disable":
		return update(func(r *accounts.Record) { r.Enabled = false }), nil
	case "set_weight":
		w, ok := intParam(b.Params, "weight")
		if !ok || w < 1 || w > 100 {
			return nil, errors.New("params.weight must be 1..100")
		}
		return update(func(r *accounts.Record) { r.Weight = w }), nil
	case "set_note":
		n, _ := b.Params["note"].(string)
		if len([]rune(n)) > 200 {
			return nil, errors.New("params.note is limited to 200 characters")
		}
		n = strings.TrimSpace(n)
		return update(func(r *accounts.Record) { r.Note = n }), nil
	case "set_proxy_slot":
		s, _ := b.Params["proxy_slot"].(string)
		s = strings.TrimSpace(s)
		return update(func(r *accounts.Record) { r.ProxySlot = s }), nil
	case "set_product":
		p, _ := b.Params["product"].(string)
		if pp := (patch{Product: &p}); pp.validate() != nil {
			return nil, errors.New("params.product must be workbuddy, vscode or cli")
		}
		np := accounts.NormalizeProduct(p)
		return update(func(r *accounts.Record) { r.Product = np }), nil
	case "clear_cooldown", "clear_restriction":
		withRestriction := b.Action == "clear_restriction"
		return func(ctx context.Context, uid string) error {
			acc := a.Pool.Get(uid)
			if acc == nil {
				return errors.New("account no longer exists")
			}
			if rec := acc.ClearCooldowns(withRestriction); rec != nil {
				return a.save(rec)
			}
			return nil
		}, nil
	case "delete":
		return func(ctx context.Context, uid string) error {
			acc := a.Pool.Get(uid)
			if acc == nil {
				return errors.New("account no longer exists")
			}
			rec := acc.Record()
			a.Pool.Remove(uid)
			if a.Delete != nil {
				return a.Delete(rec)
			}
			return nil
		}, nil
	case "refresh_credits", "checkin", "daily_chat", "daily_chat_web", "report_activity", "sync_profile", "test":
		return a.upstreamAction(b)
	case "refresh_token":
		if a.Refresher == nil {
			return nil, errors.New("token refresh is not available")
		}
		return func(ctx context.Context, uid string) error {
			acc := a.Pool.Get(uid)
			if acc == nil {
				return errors.New("account no longer exists")
			}
			return a.Refresher.Refresh(ctx, acc)
		}, nil
	}
	return nil, fmt.Errorf("unknown action %q", b.Action)
}

func (a *Accounts) batch(w http.ResponseWriter, r *http.Request) {
	var b batchReq
	if !readBody(w, r, &b) {
		return
	}
	if msg, ok := notYet[b.Action]; ok {
		writeErr(w, http.StatusNotImplemented, msg)
		return
	}
	fn, err := a.batchFn(b)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if destructive[b.Action] && !b.Confirm {
		writeErr(w, http.StatusBadRequest, b.Action+" needs \"confirm\": true")
		return
	}
	targets, err := a.resolveTargets(b, a.now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(targets) == 0 {
		writeErr(w, http.StatusBadRequest, "no accounts match")
		return
	}
	if len(targets) > maxBatch {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("at most %d accounts per batch", maxBatch))
		return
	}
	job := a.Jobs.Start(b.Action, targets, fn)
	writeJSON(w, http.StatusAccepted, job)
}

func (a *Accounts) job(w http.ResponseWriter, r *http.Request) {
	j, ok := a.Jobs.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no such job")
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (a *Accounts) jobCancel(w http.ResponseWriter, r *http.Request) {
	if !a.Jobs.Cancel(r.PathValue("id")) {
		writeErr(w, http.StatusNotFound, "no such job")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Accounts) jobList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"jobs": a.Jobs.List()})
}

// ------------------------------------------------------------------- events

// StartEvents publishes account changes to the event bus: every tick the
// accounts that changed since the last tick are sent as ONE "account.patch"
// message, however often each of them changed.
func (a *Accounts) StartEvents(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = time.Second
	}
	a.Pool.OnChange(a.changes.Mark)
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.flushChanges()
			}
		}
	}()
}

func (a *Accounts) flushChanges() {
	uids := a.changes.Take()
	if len(uids) == 0 || a.Bus == nil {
		return
	}
	now := a.now()
	var changed []map[string]any
	var removed []string
	for _, uid := range uids {
		if acc := a.Pool.Get(uid); acc != nil {
			changed = append(changed, a.view(acc, now, false))
		} else {
			removed = append(removed, uid)
		}
	}
	a.Bus.Publish("account.patch", map[string]any{"accounts": changed, "removed": removed})
}

// Register mounts the endpoints; wrap is panelauth.Auth.Require.
func (a *Accounts) Register(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	h := func(f http.HandlerFunc) http.Handler { return wrap(f) }
	mux.Handle("GET /api/v2/accounts", h(a.list))
	mux.Handle("GET /api/v2/accounts/summary", h(a.summary))
	mux.Handle("POST /api/v2/accounts/batch", h(a.batch))
	mux.Handle("GET /api/v2/accounts/{uid}", h(a.get))
	mux.Handle("PATCH /api/v2/accounts/{uid}", h(a.patch))
	mux.Handle("DELETE /api/v2/accounts/{uid}", h(a.remove))
	mux.Handle("GET /api/v2/jobs", h(a.jobList))
	mux.Handle("GET /api/v2/jobs/{id}", h(a.job))
	mux.Handle("DELETE /api/v2/jobs/{id}", h(a.jobCancel))
	if a.Bus != nil {
		mux.Handle("GET /api/v2/events", wrap(a.Bus.Handler(20*time.Second)))
	}
}

func normaliseModels(list []any) []string { return realm.NormaliseDisabledModels(list) }
