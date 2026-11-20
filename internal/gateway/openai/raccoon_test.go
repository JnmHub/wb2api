package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/refresh"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream/identity"
	"wb2api/internal/upstream/raccoon"
)

// TestRaccoonModelsDoNotLeakWorkBuddyCatalogue is the regression that keeps a
// raccoon API key from discovering the WorkBuddy model list.
func TestRaccoonModelsDoNotLeakWorkBuddyCatalogue(t *testing.T) {
	p := pool.New([]*accounts.Record{
		{UID: "rc-1", Realm: realm.Raccoon, AccessToken: "t"},
	})
	info := &Info{Chat: &Handler{Pool: p}}
	models, meta := info.raccoonModels(realm.Raccoon)
	if meta.Source != "raccoon" {
		t.Fatalf("meta source = %q", meta.Source)
	}
	if len(models) == 0 {
		t.Fatal("no raccoon models advertised")
	}
	var ids []string
	for _, m := range models {
		id, _ := m.Get("id").(string)
		ids = append(ids, id)
		if strings.HasPrefix(id, "gpt-") || strings.HasPrefix(id, "claude-") || strings.HasPrefix(id, "gemini-") {
			t.Fatalf("WorkBuddy model leaked into the raccoon catalogue: %s", id)
		}
		if id == raccoon.ImageModelID {
			t.Fatal("the image model must not be advertised while /v1/images is missing")
		}
	}
	joined := strings.Join(ids, ",")
	for _, want := range []string{"sn-glm-5-3", "sn-kimi-k3"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected %s in %s", want, joined)
		}
	}
	// The hidden Raccoon-Work ids stay out of the public list.
	if strings.Contains(joined, "raccoon-405a1c") {
		t.Fatalf("hidden model advertised: %s", joined)
	}

	// Once an account has been discovered, only its own list is advertised.
	p.Update("rc-1", func(r *accounts.Record) {
		r.Extra = map[string]any{raccoon.ExtraKey: map[string]any{"models": []any{"sn-sensenova-6-8-flash"}}}
	})
	models, _ = info.raccoonModels(realm.Raccoon)
	if len(models) != 1 {
		t.Fatalf("discovered catalogue not used: %d entries", len(models))
	}
	if id, _ := models[0].Get("id").(string); id != "sn-sensenova-6-8-flash" {
		t.Fatalf("advertised %q, want the discovered model", id)
	}
}

func TestRaccoonChatHeaders(t *testing.T) {
	cred := raccoon.Credential{Token: "tok", OrgCode: "org-1"}
	h := raccoon.ChatHeaders(cred, "relay_js_1_abc")
	get := func(name string) string {
		v, _ := h.Get(name)
		return v
	}
	if get("Authorization") != "Bearer tok" {
		t.Fatalf("authorization = %q", get("Authorization"))
	}
	if get("X-Org-Code") != "org-1" {
		t.Fatalf("org header = %q", get("X-Org-Code"))
	}
	if get("X-Client-Name") != raccoon.ClientName || get("X-Client-Version") != raccoon.ClientVersion {
		t.Fatalf("client identity headers missing: %+v", h)
	}
	if get("Accept") != "*/*" || get("Content-Type") != "application/json" {
		t.Fatalf("content headers = %q / %q", get("Accept"), get("Content-Type"))
	}
	if get("X-Request-Id") != "relay_js_1_abc" {
		t.Fatalf("request id = %q", get("X-Request-Id"))
	}
	// A personal account sends no organisation header at all.
	personal := raccoon.ChatHeaders(raccoon.Credential{Token: "tok", OrgCode: "personal"}, "")
	if _, ok := personal.Get("X-Org-Code"); ok {
		t.Fatal("a personal identity must not send X-Org-Code")
	}
	if id, _ := personal.Get("X-Request-Id"); !strings.HasPrefix(id, "relay_js_") {
		t.Fatalf("generated request id = %q", id)
	}
}

func TestRaccoonCatalogueFallbackRequiresFreshEligibleAccount(t *testing.T) {
	admin := raccoonAccount("hidden", "raccoon-090dd3")
	fields := admin.RaccoonFields()
	fields["models_visible"] = []any{}
	fields["models_at"] = json.Number("1795000000")
	for _, tc := range []struct {
		name    string
		records []*accounts.Record
		want    bool
		at      float64
	}{
		{name: "empty pool"},
		{name: "fresh account", records: []*accounts.Record{raccoonAccount("fresh")}, want: true},
		{name: "no visible chat models", records: []*accounts.Record{admin}, at: 1795000000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &Info{Chat: &Handler{Pool: pool.New(tc.records)}}
			models, meta := info.raccoonModels(realm.Raccoon)
			if (len(models) > 0) != tc.want || meta.FetchedAt != tc.at {
				t.Fatalf("models=%d fetched_at=%v", len(models), meta.FetchedAt)
			}
		})
	}
}

func TestRaccoonRedactsNestedCredentials(t *testing.T) {
	rec := raccoonAccount("raccoon", raccoon.ProbeModel)
	fields := rec.RaccoonFields()
	fields["access_token"] = "Bearer isolated-access-token"
	fields["refresh_token"] = "isolated-refresh-token"
	line := redactSecrets("echo isolated-access-token isolated-refresh-token", rec)
	if strings.Contains(line, "isolated-") {
		t.Fatal("upstream error contains nested account credentials")
	}
}

func TestChatPathPerChannel(t *testing.T) {
	cases := map[string]string{
		realm.Intl:     ChatPath,
		realm.CN:       ChatPath,
		realm.Raccoon:  raccoon.ChatPath,
		realm.CodeArts: "/api/v2/chat/completions",
	}
	for r, want := range cases {
		if got := chatPathFor(r); got != want {
			t.Fatalf("chatPathFor(%q) = %q, want %q", r, got, want)
		}
	}
	// The raccoon base already carries the /api/web/llm/v2 prefix.
	base, domain := endpointForTest(realm.Raccoon)
	if base != realm.RaccoonChatBase || domain != "" {
		t.Fatalf("raccoon endpoint = %q / %q", base, domain)
	}
}

func endpointForTest(r string) (string, string) { return identity.Endpoint(r, identity.ProductDesktop) }

// TestProbeModelPicksChannelModel locks the probe's model choice: a name the
// channel does not serve must be replaced, otherwise the probe sends a body the
// channel refuses (and the panel shows a body error instead of a real test).
func TestProbeModelPicksChannelModel(t *testing.T) {
	rec := &accounts.Record{UID: "rc-1", Realm: realm.Raccoon,
		Extra: map[string]any{raccoon.ExtraKey: map[string]any{
			"models":         []any{"sn-glm-5-3", "raccoon-090dd3"},
			"models_visible": []any{"sn-glm-5-3"},
		}}}
	if got := probeModel(rec, "deepseek-v4.1-flash"); got != "sn-glm-5-3" {
		t.Fatalf("probe model = %q, want the channel's first visible model", got)
	}
	// The catalogue here has no deepseek entry, so the default falls back to
	// the first visible model.
	if got := probeModel(rec, ""); got != "sn-glm-5-3" {
		t.Fatalf("empty model = %q, want sn-glm-5-3", got)
	}
	if got := probeModel(rec, "sn-glm-5-3"); got != "sn-glm-5-3" {
		t.Fatalf("served model must be kept, got %q", got)
	}
	// A channel with no catalogue still resolves the vendor-neutral alias and
	// probes with the channel's default model, never with a WorkBuddy name.
	bare := &accounts.Record{UID: "rc-2", Realm: realm.Raccoon}
	if got := probeModel(bare, "deepseek-v4.1-flash"); got != raccoon.ProbeModel {
		t.Fatalf("aliased probe = %q, want %q", got, raccoon.ProbeModel)
	}
	if got := probeModel(bare, ""); got != raccoon.ProbeModel {
		t.Fatalf("empty probe = %q, want the channel default %q", got, raccoon.ProbeModel)
	}
	// A WorkBuddy-only name is still replaced by something the channel serves.
	if got := probeModel(bare, "gpt-5.5"); got != raccoon.ProbeModel {
		t.Fatalf("foreign model probe = %q, want %q", got, raccoon.ProbeModel)
	}
	// Other channels keep the caller's choice.
	cn := &accounts.Record{UID: "cn-1", Realm: realm.CN}
	if got := probeModel(cn, "deepseek-v4.1-flash"); got != "deepseek-v4.1-flash" {
		t.Fatalf("workbuddy probe model changed: %q", got)
	}
}

func raccoonAccount(uid string, models ...string) *accounts.Record {
	rec := &accounts.Record{UID: uid, Realm: realm.Raccoon, AccessToken: "fixture-" + uid,
		ExpiresAt: time.Now().Add(time.Hour).Unix()}
	if len(models) > 0 {
		rec.Extra = map[string]any{raccoon.ExtraKey: map[string]any{"models": models}}
	}
	return rec
}

func TestRaccoonModelSelectionWithStickyAndPinnedAccounts(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		proxy      func(*Handler) http.Handler
	}{
		{"chat", `{"model":"sn-qwen4-max","messages":[{"role":"user","content":"hi"}]}`, func(h *Handler) http.Handler { return h }},
		{"responses", `{"model":"sn-qwen4-max","input":"hi"}`, func(h *Handler) http.Handler { return &Responses{Chat: h} }},
		{"messages", `{"model":"sn-qwen4-max","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, func(h *Handler) http.Handler { return &Messages{Chat: h} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if got := r.Header.Get("Authorization"); got != "Bearer fixture-capable" {
					t.Errorf("unexpected account %q", got)
				}
				sseUpstream("正常")(w, r)
			}), 0, `{"enable_anthropic_messages":true}`)
			e.h.Pool = pool.New([]*accounts.Record{
				raccoonAccount("incapable", "sn-glm-5-3"),
				raccoonAccount("capable", "sn-qwen4-max"),
			})
			e.h.Pool.Affinity().Bind("raccoon-test", "incapable")
			srv := httptest.NewServer(PinnedRoutes(e.h.Pool, tc.proxy(e.h)))
			defer srv.Close()
			headers := map[string]string{"X-Realm": "raccoon", "X-Session-ID": "raccoon-test"}
			resp := postJSON(t, srv.URL, tc.body, headers)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "正常") || calls.Load() != 1 {
				t.Fatalf("compatible failover: status=%d calls=%d body=%s", resp.StatusCode, calls.Load(), body)
			}
			resp = postJSON(t, srv.URL+"/u/incapable/v1/completions", tc.body, headers)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest || calls.Load() != 1 {
				t.Errorf("pinned unsupported account: status=%d calls=%d", resp.StatusCode, calls.Load())
			}
		})
	}
}

func TestRaccoonUndiscoveredModelAndResponseAliases(t *testing.T) {
	var calls atomic.Int32
	e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != raccoon.ProbeModel || !body.Stream {
			t.Errorf("unexpected outbound request: %+v (%v)", body, err)
		}
		sseUpstream("正常")(w, r)
	}), 0, "")
	e.h.Pool = pool.New([]*accounts.Record{raccoonAccount("undiscovered")})
	headers := map[string]string{"X-Realm": "raccoon"}
	resp := e.post(t, `{"model":"gpt-5.3-codex","messages":[{"role":"user","content":"hi"}]}`, headers)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || calls.Load() != 0 {
		t.Fatalf("unverified model reached upstream: status=%d calls=%d", resp.StatusCode, calls.Load())
	}
	resp = e.post(t, `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`, headers)
	defer resp.Body.Close()
	var reply struct{ Model string }
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil || resp.StatusCode != http.StatusOK || reply.Model != "deepseek-v4.1-flash" {
		t.Fatalf("raccoon reply status=%d model=%q error=%v", resp.StatusCode, reply.Model, err)
	}
}

func TestRaccoonAliasesShareStoredModelState(t *testing.T) {
	now := time.Now()
	const alias = "deepseek-v4.1-flash"
	rec := raccoonAccount("cooling", raccoon.ProbeModel)
	rec.ModelCool = map[string]float64{alias: float64(now.Add(time.Minute).Unix())}
	a := pool.NewAccount(rec)
	if a.HealthReady(raccoon.ProbeModel, now) || a.HealthReady(alias, now) {
		t.Fatal("legacy alias cooldown did not survive canonicalization")
	}
	a.ClearModelCooldown(raccoon.ProbeModel)
	if !a.HealthReady(alias, now) {
		t.Fatal("clearing canonical cooldown did not clear the alias")
	}
	a.NoteModelError(raccoon.ProbeModel, now.Add(time.Minute), time.Minute, "429", "", now)
	if a.HealthReady(alias, now) || !a.ModelCoolUntil(alias, now).After(now) {
		t.Fatal("live cooldown is not shared with the alias")
	}
	denied := raccoonAccount("denied", raccoon.ProbeModel)
	denied.DisabledModels = []string{alias}
	if a := pool.NewAccount(denied); a.HealthReady(raccoon.ProbeModel, now) || !a.ModelDisabled(raccoon.ProbeModel) {
		t.Fatal("manually disabled alias did not disable its canonical model")
	}
}

func TestRaccoonChatBaseConfiguration(t *testing.T) {
	t.Setenv("WB_RACCOON_LLM_BASE", "http://127.0.0.1:9190/local/")
	a := pool.NewAccount(raccoonAccount("test"))
	if got := (&Handler{}).chatURL(a, ""); got != raccoon.LlmBase()+raccoon.ChatPath {
		t.Fatalf("chat did not share configured model catalogue base: %q", got)
	}
	if got := (&Handler{UpstreamOverride: "http://127.0.0.1:9191/mock/"}).chatURL(a, ""); got != "http://127.0.0.1:9191/mock"+raccoon.ChatPath {
		t.Fatalf("global override precedence changed: %q", got)
	}
}

func TestRaccoonPolicyRefusalDoesNotDegradeWorkBuddy(t *testing.T) {
	var calls atomic.Int32
	e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"message":"blocked by security policy"}`, http.StatusBadRequest)
	}), 0, "")
	e.h.Pool = pool.New([]*accounts.Record{raccoonAccount("raccoon", "sn-glm-5-3")})
	body := `{"model":"sn-glm-5-3","messages":[{"role":"user","content":"hi"}]}`
	resp := e.post(t, body, map[string]string{"X-Realm": "raccoon"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || calls.Load() != 1 || e.h.degrade().Active(time.Now()) {
		t.Fatalf("policy refusal: status=%d calls=%d degraded=%t", resp.StatusCode, calls.Load(), e.h.degrade().Active(time.Now()))
	}
}

func TestRaccoonRateLimitReportsRetryAfter(t *testing.T) {
	e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":"rate_limit","message":"rate limit"}}`, http.StatusTooManyRequests)
	}), 0, "")
	e.h.Pool = pool.New([]*accounts.Record{raccoonAccount("raccoon", "sn-glm-5-3")})
	resp := e.post(t, `{"model":"sn-glm-5-3","messages":[{"role":"user","content":"hi"}]}`, map[string]string{"X-Realm": "raccoon"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("rate limit: status=%d Retry-After=%q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

type raccoonTestRefresher func(context.Context, *pool.Account) error

func (r raccoonTestRefresher) Refresh(ctx context.Context, a *pool.Account) error { return r(ctx, a) }

func TestRaccoonRefreshFailureKeepsRenewableCredentials(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		login bool
	}{
		{"refresh rate limited", errors.New("refresh HTTP 429"), false},
		{"terminal credential rejected", refresh.ErrNeedsLogin, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"code":200003,"message":"expired"}`, http.StatusUnauthorized)
			}), 0, "")
			rec := raccoonAccount("test", "sn-glm-5-3")
			rec.RefreshToken = "fixture-refresh"
			e.h.Pool = pool.New([]*accounts.Record{rec})
			e.h.Refresher = raccoonTestRefresher(func(context.Context, *pool.Account) error { return tc.err })
			resp := e.post(t, `{"model":"sn-glm-5-3","messages":[{"role":"user","content":"hi"}]}`, map[string]string{"X-Realm": "raccoon"})
			resp.Body.Close()
			a := e.h.Pool.Get(rec.UID)
			if a.Record().RaccoonNeedsLogin() != tc.login || a.TokenBad() != tc.login {
				t.Fatalf("HTTP %d: wrong credential state (needs login=%t)", resp.StatusCode, a.Record().RaccoonNeedsLogin())
			}
		})
	}
}

func TestWorkBuddyNonStreamKeepsUpstreamModel(t *testing.T) {
	e := newEnv(t, sseUpstream("ok"), 1, "")
	resp := e.post(t, `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`, nil)
	defer resp.Body.Close()
	var reply struct{ Model string }
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil || resp.StatusCode != http.StatusOK || reply.Model != "m" {
		t.Fatalf("WorkBuddy reply status=%d model=%q error=%v", resp.StatusCode, reply.Model, err)
	}
}
