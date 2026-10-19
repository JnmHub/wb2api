package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"wb2api/internal/catalog"
	"wb2api/internal/guards"
	"wb2api/internal/pool"
	"wb2api/internal/realmswitch"
	"wb2api/internal/store/settings"
)

// Settings serves the settings page: realm switch, API keys, limits, switches,
// timeouts, prompt mode, proxy slots and the model catalogue.
type Settings struct {
	Store   *settings.Store
	Pool    *pool.Pool
	Realm   *realmswitch.Switch
	Guards  *guards.Scanner
	Catalog *catalog.Service
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeN(w, r, v, 1<<20)
}

// decodeN is decode with an explicit body ceiling: imports can legitimately be
// a few megabytes (thousands of accounts), while every other write stays small.
func decodeN(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, limit))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func mask(secret string) string {
	if secret == "" {
		return ""
	}
	if len(secret) <= 10 {
		return strings.Repeat("•", len(secret))
	}
	return secret[:4] + "…" + secret[len(secret)-4:]
}

func keyView(k settings.APIKey) map[string]any {
	return map[string]any{
		"id": k.ID, "name": k.Name, "key_preview": mask(k.Key), "realm": k.Realm, "models": k.Models,
		"concurrency_limit": k.ConcurrencyLimit, "enabled": k.Enabled, "created_at": k.CreatedAt,
		"deleted_at": k.DeletedAt, "deleted": k.DeletedAt != "",
	}
}

var switchNames = []string{
	"auto_switch_product", "egress_backoff_enabled", "free_model_credit_fallback", "enable_anthropic_messages",
	"auth_disabled", "remaining_priority_enabled", "local_web_tools",
}

func (s *Settings) snapshot() map[string]any {
	d := s.Store.View()
	keys := []map[string]any{}
	for _, k := range s.Store.APIKeys(true) {
		keys = append(keys, keyView(k))
	}
	switches := map[string]bool{}
	for _, n := range switchNames {
		b, _ := d[n].(bool)
		switches[n] = b
	}
	up := s.Store.UpstreamConfig()
	slots := s.Store.ProxySlots()
	if slots == nil {
		slots = []settings.ProxySlot{}
	}
	out := map[string]any{
		"realm":                map[string]any{"current": s.Realm.Get(), "options": []string{"intl", "cn"}},
		"keys":                 keys,
		"auth_required":        !s.Store.AuthDisabled() && len(s.Store.APIKeys(false)) > 0,
		"limits":               s.Store.Limits(),
		"switches":             switches,
		"auto_switch_products": s.Store.AutoSwitchProducts(),
		"upstream": map[string]any{
			"header_timeout_seconds": int(up.HeaderTimeout.Seconds()), "idle_timeout_seconds": int(up.IdleTimeout.Seconds()),
			"device_token_set": up.DeviceToken != "", "device_token_file": up.DeviceTokenFile,
		},
		"prompt":                map[string]any{"mode": s.Store.PromptConfig().Mode, "file": s.Store.PromptConfig().File},
		"credits_refresh_hours": s.Store.CreditsRefreshHours(),
		"proxy_slots":           slots,
		"window_probe":          s.Store.WindowProbe(),
		"quota_scan_models":     s.Store.QuotaScanModels(),
	}
	if s.Catalog != nil {
		out["catalog"] = s.Catalog.Status()
	}
	return out
}

func (s *Settings) get(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.snapshot())
}

// applyRuntime pushes settings that the pool or guards cache into them.
func (s *Settings) applyRuntime() {
	var slots []pool.ProxySlot
	for _, sl := range s.Store.ProxySlots() {
		slots = append(slots, pool.ProxySlot{ID: sl.ID, URL: sl.URL, Enabled: sl.Enabled})
	}
	s.Pool.ApplyProxySlots(slots)
	if s.Guards != nil {
		s.Guards.Apply(true)
	}
}

type settingsPatch struct {
	Realm               *string                `json:"realm"`
	Switches            map[string]bool        `json:"switches"`
	AutoSwitchProducts  *[]string              `json:"auto_switch_products"`
	Upstream            map[string]any         `json:"upstream"`
	Prompt              map[string]any         `json:"prompt"`
	CreditsRefreshHours *float64               `json:"credits_refresh_hours"`
	Limits              map[string]limitChange `json:"limits"`
	WindowProbe         *bool                  `json:"window_probe"`
	QuotaScanModels     *[]string              `json:"quota_scan_models"`
}

type limitChange struct {
	Global *json.Number `json:"global"`
	Intl   *json.Number `json:"intl"`
	CN     *json.Number `json:"cn"`
	// Clear lists realm overrides to reset to "inherit".
	Clear []string `json:"clear"`
}

func (s *Settings) patch(w http.ResponseWriter, r *http.Request) {
	var p settingsPatch
	if !decode(w, r, &p) {
		return
	}
	fail := func(err error) { writeErr(w, http.StatusBadRequest, err.Error()) }
	if p.Realm != nil && !s.Realm.Set(*p.Realm) {
		writeErr(w, http.StatusBadRequest, "realm must be intl or cn")
		return
	}
	for k, v := range p.Switches {
		if err := s.Store.SetSwitch(k, v); err != nil {
			fail(err)
			return
		}
	}
	if p.AutoSwitchProducts != nil {
		if _, err := s.Store.SetAutoSwitchProducts(*p.AutoSwitchProducts); err != nil {
			fail(err)
			return
		}
	}
	if p.Upstream != nil {
		for k, v := range p.Upstream {
			if n, ok := v.(json.Number); ok {
				if i, err := n.Int64(); err == nil {
					p.Upstream[k] = int(i)
				}
			}
		}
		if _, err := s.Store.SetUpstream(p.Upstream); err != nil {
			fail(err)
			return
		}
	}
	if p.Prompt != nil {
		if _, err := s.Store.SetPrompt(p.Prompt); err != nil {
			fail(err)
			return
		}
	}
	if p.CreditsRefreshHours != nil {
		if err := s.Store.SetCreditsRefreshHours(*p.CreditsRefreshHours); err != nil {
			fail(err)
			return
		}
	}
	if p.WindowProbe != nil {
		if err := s.Store.SetWindowProbe(*p.WindowProbe); err != nil {
			fail(err)
			return
		}
	}
	if p.QuotaScanModels != nil {
		if _, err := s.Store.SetQuotaScanModels(*p.QuotaScanModels); err != nil {
			fail(err)
			return
		}
	}
	for key, ch := range p.Limits {
		set := func(scope string, n *json.Number) error {
			if n == nil {
				return nil
			}
			var v *int
			if i, err := n.Int64(); err == nil {
				x := int(i)
				v = &x
			}
			_, err := s.Store.SetLimit(key, scope, v)
			return err
		}
		for scope, n := range map[string]*json.Number{"global": ch.Global, "intl": ch.Intl, "cn": ch.CN} {
			if err := set(scope, n); err != nil {
				fail(err)
				return
			}
		}
		for _, scope := range ch.Clear {
			if _, err := s.Store.SetLimit(key, scope, nil); err != nil {
				fail(err)
				return
			}
		}
	}
	s.applyRuntime()
	writeJSON(w, http.StatusOK, s.snapshot())
}

// ---------------------------------------------------------------- API keys

type keyBody struct {
	Name             *string   `json:"name"`
	Key              *string   `json:"key"`
	Realm            *string   `json:"realm"`
	Models           *[]string `json:"models"`
	ConcurrencyLimit *int      `json:"concurrency_limit"`
	Enabled          *bool     `json:"enabled"`
}

func (b keyBody) input() settings.KeyInput {
	return settings.KeyInput{Name: b.Name, Key: b.Key, Realm: b.Realm, Models: b.Models,
		ConcurrencyLimit: b.ConcurrencyLimit, Enabled: b.Enabled}
}

func (s *Settings) createKey(w http.ResponseWriter, r *http.Request) {
	var b keyBody
	if !decode(w, r, &b) {
		return
	}
	k, err := s.Store.CreateKey(b.input())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	v := keyView(k)
	v["key"] = k.Key // shown in full exactly once, here
	writeJSON(w, http.StatusCreated, v)
}

func (s *Settings) updateKey(w http.ResponseWriter, r *http.Request) {
	var b keyBody
	if !decode(w, r, &b) {
		return
	}
	id := r.PathValue("id")
	if err := s.Store.UpdateKey(id, b.input()); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	for _, k := range s.Store.APIKeys(true) {
		if k.ID == id {
			writeJSON(w, http.StatusOK, keyView(k))
			return
		}
	}
	writeErr(w, http.StatusNotFound, "unknown key")
}

func (s *Settings) deleteKey(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteKey(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Settings) revealKey(w http.ResponseWriter, r *http.Request) {
	secret, ok := s.Store.RevealKey(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown or deleted key")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": secret})
}

// ------------------------------------------------------------- proxy slots

func (s *Settings) putSlots(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Slots []settings.ProxySlot `json:"slots"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := s.Store.SetProxySlots(in.Slots)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.applyRuntime()
	if out == nil {
		out = []settings.ProxySlot{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"slots": out})
}

// ----------------------------------------------------------------- catalog

func (s *Settings) refreshCatalog(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Realm string `json:"realm"`
	}
	if !decode(w, r, &in) {
		return
	}
	res := s.Catalog.Refresh(context.WithoutCancel(r.Context()), in.Realm)
	status := http.StatusOK
	if !res.OK {
		status = http.StatusConflict
		if res.Cooldown {
			status = http.StatusTooManyRequests
		}
	}
	writeJSON(w, status, res)
}

// Register adds the routes; wrap applies panel authentication.
func (s *Settings) Register(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	h := func(f http.HandlerFunc) http.Handler { return wrap(f) }
	mux.Handle("GET /api/v2/settings", h(s.get))
	mux.Handle("PATCH /api/v2/settings", h(s.patch))
	mux.Handle("POST /api/v2/keys", h(s.createKey))
	mux.Handle("PATCH /api/v2/keys/{id}", h(s.updateKey))
	mux.Handle("DELETE /api/v2/keys/{id}", h(s.deleteKey))
	mux.Handle("GET /api/v2/keys/{id}/secret", h(s.revealKey))
	mux.Handle("PUT /api/v2/proxy-slots", h(s.putSlots))
	if s.Catalog != nil {
		mux.Handle("POST /api/v2/catalog/refresh", h(s.refreshCatalog))
	}
}
