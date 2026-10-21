package openai

import (
	"encoding/json"
	"math"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"wb2api/internal/catalog"
	"wb2api/internal/jsonx"
	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/settings"
)

// Info serves the read-only gateway endpoints: /v1/models (and /models),
// /v1/balance-summary and /health.
type Info struct {
	// Chat supplies API-key authentication and the default realm.
	Chat    *Handler
	Pool    *pool.Pool
	Catalog *catalog.Service
}

// Register adds the routes to mux.
func (i *Info) Register(mux *http.ServeMux) {
	mux.HandleFunc("/v1/models", i.models)
	mux.HandleFunc("/models", i.models)
	mux.HandleFunc("/v1/balance-summary", i.balance)
	mux.HandleFunc("/health", i.health)
}

func (i *Info) defaultRealm() string { return i.Chat.activeRealm() }

// requestRealm mirrors the chat path: the realm bound to the API key, then
// X-Realm, then ?realm=, then the pinned account's realm, then the active one.
// "all" is accepted for listings. Unknown values fall back so a client cannot
// grow the per-realm cache.
func (i *Info) requestRealm(r *http.Request, key *settings.APIKey) string {
	bound := ""
	if key != nil {
		bound = key.Realm
	}
	for _, c := range []string{bound, r.Header.Get("X-Realm"), r.URL.Query().Get("realm")} {
		switch c {
		case realm.Intl, realm.CN, "all":
			return c
		}
	}
	if p := PinnedFrom(r.Context()); p != nil {
		return p.Realm
	}
	return i.defaultRealm()
}

func readOnly(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return false
	}
	return true
}

func writeObj(w http.ResponseWriter, r *http.Request, status int, v any) {
	b, err := jsonx.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot encode response", "server_error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(append(b, '\n'))
	}
}

// pyFloat spells a float like Python's repr (100.0, not 100).
func pyFloat(f float64) json.Number {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return json.Number(s)
}

func (i *Info) authorize(w http.ResponseWriter, r *http.Request) (*settings.APIKey, bool) {
	k, ok := i.Chat.Authenticate(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing or invalid API key", "invalid_request_error")
		return nil, false
	}
	return k, true
}

func (i *Info) models(w http.ResponseWriter, r *http.Request) {
	if !readOnly(w, r) {
		return
	}
	boundKey, ok := i.authorize(w, r)
	if !ok {
		return
	}
	rl := i.requestRealm(r, boundKey)
	data, meta := i.Catalog.Entries(r.Context(), rl)
	list := make([]any, 0, len(data))
	pinned := PinnedFrom(r.Context())
	filterByKey := i.Chat != nil && i.Chat.Settings != nil && i.Chat.Settings.FilterModelsByKey()
	for _, d := range data {
		id, _ := d.Get("id").(string)
		// a local deny-list keeps blocked models out of every client's picker
		if i.Chat != nil && i.Chat.Settings != nil && i.Chat.Settings.ModelBanned(id) {
			continue
		}
		if filterByKey && boundKey != nil && len(boundKey.Models) > 0 && !boundKey.AllowsModel(id) {
			continue
		}
		// a pinned route lists what that account can actually take
		if pinned != nil && pinned.ModelDisabled(id) {
			continue
		}
		list = append(list, d)
	}
	out := jsonx.NewObj()
	out.Set("object", "list")
	out.Set("data", list)
	out.Set("realm", rl)
	m := jsonx.NewObj()
	m.Set("source", meta.Source)
	m.Set("fetched_at", pyFloat(meta.FetchedAt))
	if meta.FileMtime != nil {
		m.Set("file_mtime", pyFloat(*meta.FileMtime))
	} else {
		m.Set("file_mtime", nil)
	}
	out.Set("catalog", m)
	writeObj(w, r, http.StatusOK, out)
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// balance sums the cached credit snapshots without any account-identifying
// field. It never refreshes credits upstream: a balance widget may poll this
// often. Unknown snapshots stay unknown instead of reading as zero.
func (i *Info) balance(w http.ResponseWriter, r *http.Request) {
	if !readOnly(w, r) {
		return
	}
	boundKey, ok := i.authorize(w, r)
	if !ok {
		return
	}
	rl := i.requestRealm(r, boundKey)
	enabled, known := 0, 0
	total := 0.0
	var updated float64
	haveStamp := false
	for _, a := range i.Pool.All() {
		rec := a.Record()
		if !a.Enabled() || (rl != "all" && rec.Realm != rl) {
			continue
		}
		enabled++
		remain, ok := toFloat(rec.Credits["remain"])
		if !ok || !finite(remain) || remain < 0 {
			continue
		}
		total += remain
		known++
		if st, ok := toFloat(rec.Credits["updated_at"]); ok && finite(st) {
			if !haveStamp || st > updated {
				updated, haveStamp = st, true
			}
		}
	}
	out := jsonx.NewObj()
	if known > 0 {
		rounded, _ := strconv.ParseFloat(strconv.FormatFloat(total, 'f', 2, 64), 64)
		out.Set("balance", pyFloat(rounded))
	} else {
		out.Set("balance", nil)
	}
	out.Set("unit", "credit")
	out.Set("realm", rl)
	out.Set("accounts_enabled", json.Number(strconv.Itoa(enabled)))
	out.Set("accounts_with_credit_data", json.Number(strconv.Itoa(known)))
	if haveStamp {
		out.Set("updated_at", pyFloat(updated))
	} else {
		out.Set("updated_at", nil)
	}
	writeObj(w, r, http.StatusOK, out)
}

// health always answers (launchers use it to detect a running copy) but only
// shows account identity to an authorised caller.
func (i *Info) health(w http.ResponseWriter, r *http.Request) {
	if !readOnly(w, r) {
		return
	}
	_, authed := i.Chat.Authenticate(r)
	ready := 0
	for _, rl := range []string{realm.Intl, realm.CN} {
		ready += i.Pool.ReadyCount(rl, "")
	}
	out := jsonx.NewObj()
	out.Set("ok", true)
	out.Set("status", "ok")
	out.Set("realm", i.defaultRealm())
	out.Set("accounts", json.Number(strconv.Itoa(i.Pool.Len())))
	out.Set("accounts_ready", json.Number(strconv.Itoa(ready)))
	out.Set("api_key_required", i.Chat.KeyRequired())
	if authed {
		var rep *pool.Account
		for _, a := range i.Pool.All() {
			if a.Record().AccessToken != "" {
				rep = a
				break
			}
			if rep == nil {
				rep = a
			}
		}
		if rep == nil {
			for _, k := range []string{"uid", "domain", "issuer", "credential_file", "expires_at"} {
				out.Set(k, nil)
			}
		} else {
			rec := rep.Record()
			out.Set("uid", rec.UID)
			out.Set("domain", rec.Domain)
			out.Set("issuer", realm.JWTIssuer(rec.AccessToken))
			if rec.Path != "" {
				out.Set("credential_file", filepath.Base(rec.Path))
			} else {
				out.Set("credential_file", nil)
			}
			out.Set("expires_at", json.Number(strconv.FormatInt(rec.ExpiresAt, 10)))
		}
	}
	writeObj(w, r, http.StatusOK, out)
}
