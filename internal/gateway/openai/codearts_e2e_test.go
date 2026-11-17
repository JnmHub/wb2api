package openai

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
	"wb2api/internal/store/settings"
	"wb2api/internal/upstream"
)

// codeartsEnv wires a gateway whose pool holds one CodeArts account and whose
// upstream is a fake snap-access endpoint.
type codeartsEnv struct {
	h      *Handler
	srv    *httptest.Server
	events chan UsageEvent
	hits   atomic.Int32
	// last request capture
	path   string
	header http.Header
	body   []byte
}

func newCodeartsEnv(t *testing.T, up http.HandlerFunc) *codeartsEnv {
	t.Helper()
	e := &codeartsEnv{events: make(chan UsageEvent, 16)}
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hits.Add(1)
		e.path = r.URL.Path
		e.header = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		e.body = body
		up(w, r)
	}))
	t.Cleanup(upSrv.Close)

	rec := &accounts.Record{
		UID: "hw-1", Nickname: "hw", Realm: realm.CodeArts, AccessToken: "STS-TOKEN",
		ExpiresAt: time.Now().Add(48 * time.Hour).Unix(),
		Extra: map[string]any{"codearts": map[string]any{
			"access_key_id": "AK-HW", "secret_access_key": "SK-HW",
			"models": []any{"GLM-5.2"},
		}},
	}
	e.h = &Handler{
		Pool:             pool.New([]*accounts.Record{rec}),
		Up:               upstream.NewManager(upstream.DefaultTimeouts()),
		Settings:         settings.Open(t.TempDir()),
		UpstreamOverride: upSrv.URL,
		MaxAttempts:      2,
		OnUsage:          func(ev UsageEvent) { e.events <- ev },
	}
	e.srv = httptest.NewServer(e.h)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *codeartsEnv) post(t *testing.T, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Realm", realm.CodeArts)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func codeartsSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, `data: {"id":"up-1","object":"chat.completion.chunk","created":1790000000,"model":"GLM-5.2","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":""}]}`+"\n\n")
	fmt.Fprint(w, `data: {"id":"up-1","object":"chat.completion.chunk","created":1790000000,"model":"GLM-5.2","choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":""}]}`+"\n\n")
	fmt.Fprint(w, `data: {"id":"up-1","object":"chat.completion.chunk","created":1790000000,"model":"GLM-5.2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
	fmt.Fprint(w, `data: {"id":"up-1","choices":[],"usage":{"prompt_tokens":8,"completion_tokens":2,"total_tokens":10}}`+"\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// TestCodeArtsStreamEndToEnd drives a full request through the gateway: the
// account is picked from the channel, the body is reduced to the CodeArts
// schema, the request is signed, and the upstream SSE is relayed unchanged.
func TestCodeArtsStreamEndToEnd(t *testing.T) {
	e := newCodeartsEnv(t, codeartsSSE)

	resp := e.post(t, `{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body = %s", resp.StatusCode, b)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "你好") {
		t.Fatalf("stream did not carry the upstream content: %s", raw)
	}

	if e.path != "/api/v2/chat/completions" {
		t.Fatalf("upstream path = %q", e.path)
	}
	if got := e.header.Get("x-auth-token"); got != "STS-TOKEN" {
		t.Fatalf("x-auth-token = %q", got)
	}
	if got := e.header.Get("app-id"); got != "CodeAgent3.0" {
		t.Fatalf("app-id = %q", got)
	}
	auth := e.header.Get("Authorization")
	if !strings.HasPrefix(auth, "SDK-HMAC-SHA256 Access=AK-HW, ") {
		t.Fatalf("Authorization = %q", auth)
	}
	if raw, ok := e.header["X-Sdk-Content-Sha256"]; !ok || len(raw) == 0 {
		t.Fatal("missing payload hash")
	}
	sum := sha256.Sum256(e.body)
	if got := e.header.Get("X-Sdk-Content-Sha256"); got != hex.EncodeToString(sum[:]) {
		t.Fatal("the signature does not cover the body that was sent")
	}
	if enc := e.header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("codearts body must not be compressed (signed as-is), got %q", enc)
	}
	if ua := e.header.Get("User-Agent"); strings.Contains(ua, "WorkBuddy") {
		t.Fatalf("WorkBuddy identity leaked upstream: %q", ua)
	}
	ev := <-e.events
	if ev.Outcome == "failed" {
		t.Fatalf("usage outcome = %q", ev.Outcome)
	}
	if ev.Account != "hw-1" {
		t.Fatalf("usage account = %q", ev.Account)
	}
}

// TestCodeArtsNonStreamClientAggregates covers the non-stream client path: the
// upstream is always asked to stream, and the gateway assembles the response.
func TestCodeArtsNonStreamClientAggregates(t *testing.T) {
	e := newCodeartsEnv(t, codeartsSSE)
	resp := e.post(t, `{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body = %s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), `"object":"chat.completion"`) {
		t.Fatalf("expected an aggregated completion: %s", raw)
	}
	if !strings.Contains(string(raw), "你好") {
		t.Fatalf("aggregated content missing: %s", raw)
	}
	// The wire request is still a stream, because that is the only shape the
	// upstream speaks.
	var body struct {
		Stream *bool `json:"stream"`
	}
	if err := json.Unmarshal(e.body, &body); err != nil {
		t.Fatalf("upstream body is not JSON: %v (%s)", err, e.body)
	}
	if body.Stream == nil || !*body.Stream {
		t.Fatal("upstream must be asked to stream")
	}
}

// A parameter rejection is the client's fault: the gateway relays the 400 and
// keeps the account schedulable.
func TestCodeArtsParamErrorKeepsAccountUsable(t *testing.T) {
	e := newCodeartsEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error_code":"InferHub.001001005","error_msg":"invalid parameter: tools"}`)
	})
	for i := 0; i < 2; i++ {
		resp := e.post(t, `{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`)
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("attempt %d: status = %d body = %s", i, resp.StatusCode, raw)
		}
	}
	if got := e.hits.Load(); got != 2 {
		t.Fatalf("upstream hits = %d, want 2 (a cooled account would refuse the second call)", got)
	}
	if !e.h.Pool.Get("hw-1").Ready("GLM-5.2", time.Now()) {
		t.Fatal("a request-shape error must not cool the account")
	}
}

// A full CodeArts account must fail over locally instead of firing a fourth
// parallel turn at an upstream that only tolerates three.
func TestCodeArtsFullAccountDoesNotReachUpstream(t *testing.T) {
	t.Setenv("WB_CODEARTS_INFLIGHT", "")
	e := newCodeartsEnv(t, codeartsSSE)

	acc := e.h.Pool.Get("hw-1")
	if acc == nil {
		t.Fatal("fixture account missing")
	}
	limit := e.h.accountLimit(acc)
	releases := make([]func(), 0, limit)
	for i := 0; i < limit; i++ {
		rel, ok := acc.TryAcquire(limit)
		if !ok {
			t.Fatalf("could not fill slot %d of %d", i, limit)
		}
		releases = append(releases, rel)
	}

	resp := e.post(t, `{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("a full account served the request anyway: %s", raw)
	}
	if hits := e.hits.Load(); hits != 0 {
		t.Fatalf("upstream was called %d times while every slot was held", hits)
	}

	for _, rel := range releases {
		rel()
	}
	resp = e.post(t, `{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("released account still refused: %d %s", resp.StatusCode, out)
	}
	if e.hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", e.hits.Load())
	}
}

// /v1/models must serve the channel's discovered catalogue and nothing from the
// WorkBuddy one.
func TestModelsEndpointListsChannelCatalogue(t *testing.T) {
	rec := &accounts.Record{
		UID: "hw-1", Realm: realm.CodeArts, AccessToken: "t",
		Extra: map[string]any{"codearts": map[string]any{
			"access_key_id": "AK", "secret_access_key": "SK",
			"models":         []any{"GLM-5.2", "Qwen3-VL-235B"},
			"benefit_models": []any{"Qwen3-VL-235B"},
		}},
	}
	h := &Handler{Pool: pool.New([]*accounts.Record{rec}), Settings: settings.Open(t.TempDir())}
	info := &Info{Chat: h, Pool: h.Pool}
	mux := http.NewServeMux()
	info.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/v1/models?realm=codearts", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	var out struct {
		Object string `json:"object"`
		Realm  string `json:"realm"`
		Data   []struct {
			ID      string `json:"id"`
			Benefit bool   `json:"benefit"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body: %v (%s)", err, w.Body.String())
	}
	if out.Realm != realm.CodeArts || len(out.Data) != 2 {
		t.Fatalf("out = %+v", out)
	}
	got := map[string]bool{}
	for _, d := range out.Data {
		got[d.ID] = d.Benefit
	}
	if _, ok := got["GLM-5.2"]; !ok {
		t.Fatalf("GLM-5.2 missing: %+v", out.Data)
	}
	if b, ok := got["Qwen3-VL-235B"]; !ok || !b {
		t.Fatalf("benefit flag missing: %+v", out.Data)
	}

	// The X-Realm header routes the same way.
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-Realm", realm.CodeArts)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("header status = %d", w.Code)
	}
}
