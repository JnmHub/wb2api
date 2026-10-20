package openai

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	"wb2api/internal/jsonx"
	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/settings"
)

type pinnedKey struct{}

// PinnedFrom returns the account a /u/<ref>/... route was pinned to (nil when
// the request was not pinned).
func PinnedFrom(ctx context.Context) *pool.Account {
	a, _ := ctx.Value(pinnedKey{}).(*pool.Account)
	return a
}

// PinnedRoutes serves /u/<ref>/v1/... : the prefix is stripped and the ref
// resolved to exactly one account (uid, nickname or uid prefix), and the
// request is then served by that account alone — no failover. Clients use it to
// make one provider per account.
func PinnedRoutes(p *pool.Pool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if !strings.HasPrefix(path, "/u/") {
			next.ServeHTTP(w, r)
			return
		}
		rest := path[3:]
		raw, tail, ok := strings.Cut(rest, "/")
		ref, err := url.PathUnescape(raw)
		ref = strings.TrimSpace(ref)
		if err != nil || ref == "" || !ok {
			writeError(w, http.StatusNotFound, "pinned account route must be /u/<ref>/v1/...", "invalid_request_error")
			return
		}
		acc, why := p.FindUnique(ref)
		if acc == nil {
			status := http.StatusNotFound
			if why == "is ambiguous" {
				status = http.StatusConflict
			}
			writeError(w, status, "pinned account '"+ref+"' "+why, "invalid_request_error")
			return
		}
		r2 := r.Clone(context.WithValue(r.Context(), pinnedKey{}, acc))
		u := *r.URL
		u.Path = "/" + tail
		u.RawPath = ""
		r2.URL = &u
		r2.RequestURI = u.RequestURI()
		next.ServeHTTP(w, r2)
	})
}

// -------------------------------------------------------- request realm

// requestRealm picks the exit for a request: the realm bound to the API key,
// then X-Realm, then ?realm=; "" when none of them says (the caller falls back
// to the active realm). Unknown values are ignored.
func requestRealm(r *http.Request, key *settings.APIKey) string {
	cands := []string{}
	if key != nil {
		cands = append(cands, key.Realm)
	}
	cands = append(cands, r.Header.Get("X-Realm"), r.URL.Query().Get("realm"))
	for _, c := range cands {
		switch c {
		case realm.Intl, realm.CN:
			return c
		}
	}
	return ""
}

func (h *Handler) activeRealm() string {
	if h.CurrentRealm != nil {
		if r := h.CurrentRealm(); r != "" {
			return r
		}
	}
	if h.DefaultRealm != "" {
		return h.DefaultRealm
	}
	return realm.Intl
}

// crossRealmMessage explains a model/exit mismatch instead of letting the
// upstream reject it with an opaque 403.
func crossRealmMessage(model, reqRealm string, key *settings.APIKey) string {
	owner := realm.ExclusiveRealm(model)
	if owner == "" || reqRealm == "" || owner == reqRealm {
		return ""
	}
	name := "当前 Key"
	if key != nil && key.Name != "" {
		name = key.Name
	}
	served, used := "国际版", "国际版"
	if owner == realm.CN {
		served = "国内版"
	}
	if reqRealm == realm.CN {
		used = "国内版"
	}
	return "模型 " + model + " 只在" + served + "提供，但「" + name + "」绑定的是" + used +
		"出口。请改用对应出口的 Key，或把该 Key 的出口改为「跟随面板切换」。"
}

// ----------------------------------------------------- per-key concurrency
//
// The gateway deliberately does not enforce a client-side concurrency cap:
// every request is forwarded and upstream limits are handled by the normal
// 429/cooldown machinery. The stored `concurrency_limit` field is kept for
// compatibility with older settings but ignored.

func pinnedRef(r *http.Request) string {
	if a := PinnedFrom(r.Context()); a != nil {
		return a.UID
	}
	return ""
}

// ----------------------------------------------- background model alias

var (
	bgTarget = envStr("WB_BACKGROUND_MODEL_TARGET", "deepseek-v4.1-flash")
	bgNames  = func() map[string]bool {
		out := map[string]bool{}
		for _, m := range strings.Split(envStr("WB_BACKGROUND_MODEL_NAMES", "gpt-5.6-terra,gpt-5.6-luna,gpt-5.3-codex"), ",") {
			if m = strings.ToLower(strings.TrimSpace(m)); m != "" {
				out[m] = true
			}
		}
		return out
	}()
)

func envStr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// applyBackgroundAlias rewrites the models a desktop client hard-codes for its
// background work (memory, ambient suggestions, automations) to a free target,
// so those features keep working without spending credits when a free model is
// picked in the UI.
func applyBackgroundAlias(payload *jsonx.Obj, log *slog.Logger) {
	name, ok := payload.Get("model").(string)
	if !ok || !bgNames[strings.ToLower(strings.TrimSpace(name))] {
		return
	}
	payload.Set("model", bgTarget)
	log.Info("alias: background model rewritten", "from", name, "to", bgTarget)
}

// keyModelMessage explains a per-key model restriction.
func keyModelMessage(key *settings.APIKey, model string) string {
	name := key.Name
	if name == "" {
		name = "未命名"
	}
	allowed := strings.Join(key.Models, "、")
	if allowed == "" {
		allowed = "-"
	}
	asked := strings.TrimSpace(model)
	if asked == "" {
		asked = "(未指定模型)"
	}
	return "API Key「" + name + "」的模型限制不允许 " + asked + "。该 Key 目前允许：" + allowed +
		"。请在看板「设置」页修改这个 Key 的模型限制，或改用允许该模型的 Key。"
}
