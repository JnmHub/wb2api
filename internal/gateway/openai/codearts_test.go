package openai

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wb2api/internal/jsonx"
	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
	"wb2api/internal/store/settings"
	"wb2api/internal/upstream/identity"
)

func codeartsAccount(uid string, models ...string) *accounts.Record {
	list := make([]any, 0, len(models))
	for _, m := range models {
		list = append(list, m)
	}
	return &accounts.Record{
		UID: uid, Realm: realm.CodeArts, AccessToken: "STS-" + uid,
		Extra: map[string]any{"codearts": map[string]any{
			"access_key_id": "AK-" + uid, "secret_access_key": "SK-" + uid,
			"models": list,
		}},
	}
}

func TestCodeArtsChatPath(t *testing.T) {
	if got := chatPathFor(realm.CodeArts); got != "/api/v2/chat/completions" {
		t.Fatalf("codearts path = %q", got)
	}
	if got := chatPathFor(realm.CN); got != ChatPath {
		t.Fatalf("workbuddy path = %q", got)
	}
}

// A CodeArts attempt must send the reduced body, uncompressed, and sign exactly
// those bytes: a gzip layer would break the payload hash.
func TestCodeArtsAttemptBodyAndSignature(t *testing.T) {
	h := &Handler{}
	payload := jsonx.NewObj()
	payload.Set("model", "GLM-5.2")
	payload.Set("stream", false)
	payload.Set("metadata", jsonx.NewObj())
	payload.Set("conversation_id", "conv-1")
	q := &request{model: "GLM-5.2", realm: realm.CodeArts, shaped: payload, sessionKey: "sess-1"}

	rec := codeartsAccount("hw1")
	out := h.buildOutbound(q, rec, identity.ProductDesktop)
	if out.gz {
		t.Fatal("a signed request must not be gzipped")
	}
	body, err := jsonx.UnmarshalObj(out.body)
	if err != nil {
		t.Fatalf("body is not a JSON object: %v", err)
	}
	if v, _ := body.Get("stream").(bool); !v {
		t.Fatal("stream must be forced true")
	}
	if body.Has("metadata") || body.Has("conversation_id") {
		t.Fatalf("WorkBuddy fields leaked upstream: %v", body.Keys())
	}
	if v, _ := body.Get("chat_id").(string); v == "" {
		t.Fatal("a session key must derive a chat id")
	}

	hdrs := h.chatHeaders(q, rec, identity.ProductDesktop, out)
	if v, _ := hdrs.Get("x-auth-token"); v != "STS-hw1" {
		t.Fatalf("x-auth-token = %q", v)
	}
	auth, ok := hdrs.Get("Authorization")
	if !ok || !strings.HasPrefix(auth, "SDK-HMAC-SHA256 Access=AK-hw1, ") {
		t.Fatalf("Authorization = %q", auth)
	}
	sum := sha256.Sum256(out.body)
	if got, _ := hdrs.Get("X-Sdk-Content-Sha256"); got != hex.EncodeToString(sum[:]) {
		t.Fatal("signature does not cover the body that is about to be sent")
	}
	if _, ok := hdrs.Get("X-User-Id"); ok {
		t.Fatal("a CodeArts request must not carry WorkBuddy identity headers")
	}
}

func TestWorkBuddyAttemptStaysGzipped(t *testing.T) {
	h := &Handler{}
	payload := jsonx.NewObj()
	payload.Set("model", "deepseek-v4.1-flash")
	payload.Set("stream", true)
	q := &request{model: "deepseek-v4.1-flash", realm: realm.CN, shaped: payload}
	q.enc = map[string]*encoded{}

	rec := &accounts.Record{UID: "cn1", Realm: realm.CN, AccessToken: "tok"}
	out := h.buildOutbound(q, rec, identity.ProductCLI)
	if !out.gz {
		t.Fatal("the WorkBuddy channels keep their gzip path")
	}
	if len(out.body) == 0 {
		t.Fatal("empty body")
	}
}

func TestRequestRealmAcceptsCodeArts(t *testing.T) {
	key := &settings.APIKey{Realm: realm.CodeArts}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if got := requestRealm(r, key); got != realm.CodeArts {
		t.Fatalf("key-bound realm = %q", got)
	}
	r2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?realm=codearts", nil)
	if got := requestRealm(r2, nil); got != realm.CodeArts {
		t.Fatalf("query realm = %q", got)
	}
	r3 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r3.Header.Set("X-Realm", "CodeArts")
	if got := requestRealm(r3, nil); got != realm.CodeArts {
		t.Fatalf("header realm = %q (must be trimmed and lower-cased)", got)
	}
	r4 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?realm=huawei", nil)
	if got := requestRealm(r4, nil); got != "" {
		t.Fatalf("unknown realm = %q, want empty", got)
	}
}

// A model name that only exists on one WorkBuddy exit must not be rejected just
// because the request is routed to another vendor: the exclusivity table does
// not describe that vendor's catalogue.
func TestCrossRealmMessageIgnoresOtherFamilies(t *testing.T) {
	key := &settings.APIKey{Name: "hw"}
	if got := crossRealmMessage("glm-5.1", realm.CodeArts, key); got != "" {
		t.Fatalf("codearts must not be blocked by a WorkBuddy exclusivity rule: %s", got)
	}
	if got := crossRealmMessage("glm-5.1", realm.Intl, key); got == "" {
		t.Fatal("the WorkBuddy cross-exit check must still fire")
	}
	if got := crossRealmMessage("deepseek-v4-pro", realm.CN, key); got != "" {
		t.Fatalf("same-exit request was rejected: %s", got)
	}
}

// /v1/models for the channel is the union of what the pool's accounts declare,
// with the benefit flag preserved.
func TestCodeArtsModelsFromPool(t *testing.T) {
	recs := []*accounts.Record{
		codeartsAccount("hw1", "GLM-5.2", "Qwen3-VL-235B"),
		codeartsAccount("hw2", "GLM-5.2", "deepseek-v4.1-flash-1017"),
		{UID: "cn1", Realm: realm.CN},
	}
	recs[0].Extra["codearts"].(map[string]any)["benefit_models"] = []any{"Qwen3-VL-235B"}
	p := pool.New(recs)
	info := &Info{Chat: &Handler{Pool: p}}

	got := info.codeartsModels(realm.CodeArts)
	ids := make([]string, 0, len(got))
	for _, e := range got {
		id, _ := e.Get("id").(string)
		ids = append(ids, id)
		if id == "Qwen3-VL-235B" {
			if v, _ := e.Get("benefit").(bool); !v {
				t.Fatal("benefit flag lost")
			}
		}
	}
	want := []string{"GLM-5.2", "Qwen3-VL-235B", "deepseek-v4.1-flash-1017"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	// A WorkBuddy account must not leak into the channel list.
	for _, id := range ids {
		if strings.HasPrefix(id, "gpt-") {
			t.Fatalf("unexpected id %q", id)
		}
	}
	if other := info.codeartsModels(realm.CN); len(other) != 0 {
		t.Fatalf("CN realm produced %d codearts models", len(other))
	}
}

// The panel-level key check must accept the new channel value; see
// internal/store/settings for the validate() test (it is package-private).

var _ = func() *Handler { return &Handler{} }

// The handler must narrow the global per-account limit for the CodeArts
// channel: three parallel turns are what the upstream tolerates.
func TestAccountLimitPerChannel(t *testing.T) {
	t.Setenv("WB_CODEARTS_INFLIGHT", "")
	h := &Handler{AccountInflight: 8}
	hw := pool.NewAccount(&accounts.Record{UID: "hw-1", Realm: realm.CodeArts, AccessToken: "t"})
	cn := pool.NewAccount(&accounts.Record{UID: "cn-1", Realm: realm.CN, AccessToken: "t"})

	if got := h.accountLimit(hw); got != realm.DefaultCodeArtsInflight {
		t.Fatalf("codearts limit = %d, want %d", got, realm.DefaultCodeArtsInflight)
	}
	if got := h.accountLimit(cn); got != 8 {
		t.Fatalf("workbuddy limit = %d, want the global 8", got)
	}
	if got := h.realmLimit(realm.CodeArts); got != realm.DefaultCodeArtsInflight {
		t.Fatalf("realm limit = %d", got)
	}
	if got := h.accountLimit(nil); got != 8 {
		t.Fatalf("nil account = %d, want the global value", got)
	}

	t.Setenv("WB_CODEARTS_INFLIGHT", "3")
	if got := h.accountLimit(hw); got != 3 {
		t.Fatalf("override = %d, want 3", got)
	}
}

// Three in-flight turns fill a CodeArts account even though the global limit
// would allow eight.
func TestCodeArtsAccountFillsAtChannelLimit(t *testing.T) {
	t.Setenv("WB_CODEARTS_INFLIGHT", "")
	h := &Handler{AccountInflight: 8}
	acc := pool.NewAccount(&accounts.Record{UID: "hw-1", Realm: realm.CodeArts, AccessToken: "t"})
	limit := h.accountLimit(acc)
	releases := make([]func(), 0, limit)
	for i := 0; i < limit; i++ {
		rel, ok := acc.TryAcquire(limit)
		if !ok {
			t.Fatalf("acquire %d failed at limit %d", i, limit)
		}
		releases = append(releases, rel)
	}
	if !acc.AtCapacity(h.accountLimit(acc)) {
		t.Fatal("a CodeArts account with three turns in flight must be full")
	}
	for _, rel := range releases {
		rel()
	}
	if acc.AtCapacity(h.accountLimit(acc)) {
		t.Fatal("released slots must free the account")
	}
}
