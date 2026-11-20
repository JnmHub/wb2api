package api

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream/codearts"
	"wb2api/internal/upstream/raccoon"
)

// TestRaccoonLoginStartPicksMode locks the two sign-in modes: the official
// authorize page only accepts an http loopback redirect with an explicit port,
// so anything else must fall back to the desktop scheme.
func TestRaccoonLoginStartPicksMode(t *testing.T) {
	cases := []struct {
		name     string
		host     string
		tls      bool
		wantMode string
	}{
		{"loopback", "127.0.0.1:8788", false, "web"},
		{"localhost", "localhost:8788", false, "web"},
		{"domain", "panel.example.com", false, "desktop"},
		{"lan ip", "10.0.0.5:8788", false, "desktop"},
		{"https", "localhost:8788", true, "desktop"},
		{"no port", "localhost", false, "desktop"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := &Operations{}
			req := httptest.NewRequest(http.MethodPost, "/api/v2/raccoon/login/start", nil)
			req.Host = c.host
			if c.tls {
				req.TLS = &tlsState
			}
			rec := httptest.NewRecorder()
			o.raccoonLoginStart(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			var out struct {
				State   string `json:"state"`
				Mode    string `json:"mode"`
				AuthURL string `json:"authUrl"`
				Hint    string `json:"hint"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if out.Mode != c.wantMode {
				t.Fatalf("mode = %q, want %q", out.Mode, c.wantMode)
			}
			if out.State == "" || !strings.Contains(out.AuthURL, "login_source="+c.wantMode) {
				t.Fatalf("authUrl = %q state=%q", out.AuthURL, out.State)
			}
			if c.wantMode == "web" && !strings.Contains(out.AuthURL, "redirect=") {
				t.Fatalf("web mode without redirect: %q", out.AuthURL)
			}
			if out.Hint == "" {
				t.Fatal("hint missing")
			}
			// The attempt is registered so the callback can be matched.
			if _, ok := o.raccoonLogins.get(out.State); !ok {
				t.Fatal("login attempt not registered")
			}
		})
	}
}

// TestRaccoonLoginCompleteRejectsForeignState proves the callback parse is
// checked before anything reaches the upstream.
func TestRaccoonLoginCompleteRejectsForeignState(t *testing.T) {
	o := &Operations{}
	o.raccoonLogins.put("state-a", "desktop")
	body := strings.NewReader(`{"state":"state-a","url":"office-raccoon://auth/callback?code=C&state=state-b"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v2/raccoon/login/complete", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	o.raccoonLoginComplete(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "state") {
		t.Fatalf("unhelpful error: %s", rec.Body.String())
	}
}

// TestRaccoonCallbackPageNeedsCode keeps the web landing page from calling the
// upstream with an empty code.
func TestRaccoonCallbackPageNeedsCode(t *testing.T) {
	o := &Operations{}
	req := httptest.NewRequest(http.MethodGet, "/api/v2/raccoon/login/callback?state=abc", nil)
	rec := httptest.NewRecorder()
	o.raccoonLoginCallback(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "授权码") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

var tlsState = tls.ConnectionState{}

func raccoonTestToken(t *testing.T, subject string) string {
	t.Helper()
	claims, err := json.Marshal(map[string]any{"sub": subject, "exp": time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return "fixture." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
}

func TestRaccoonLoginAttemptIsValidatedAndUsedOnce(t *testing.T) {
	token := raccoonTestToken(t, "fixture-user")
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != raccoon.ExchangeCodePath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]string{"access_token": token}})
	}))
	defer upstream.Close()
	t.Setenv("WB_RACCOON_AUTH_BASE", upstream.URL)
	o := &Operations{Pool: pool.New(nil)}
	o.raccoonLogins.put("web", raccoon.ModeWeb)
	o.raccoonLogins.put("desktop", raccoon.ModeDesktop)
	o.raccoonLogins.put("expired", raccoon.ModeWeb)
	o.raccoonLogins.m["expired"].created = time.Now().Add(-raccoonLoginTTL)
	o.raccoonLogins.put("cancelled", raccoon.ModeWeb)
	cancel := httptest.NewRecorder()
	o.raccoonLoginCancel(cancel, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"state":"cancelled"}`)))
	if cancel.Code != http.StatusOK || !strings.Contains(cancel.Body.String(), `"cancelled":true`) {
		t.Fatalf("cancel response: %d %s", cancel.Code, cancel.Body.String())
	}
	for _, state := range []string{"", "unknown", "desktop", "expired", "cancelled"} {
		recorder := httptest.NewRecorder()
		o.raccoonLoginCallback(recorder, httptest.NewRequest(http.MethodGet, "/?code=fixture&state="+state, nil))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("invalid attempt %q returned HTTP %d", state, recorder.Code)
		}
	}
	for _, want := range []int{http.StatusOK, http.StatusConflict} {
		recorder := httptest.NewRecorder()
		o.raccoonLoginCallback(recorder, httptest.NewRequest(http.MethodGet, "/?code=fixture&state=web", nil))
		if recorder.Code != want {
			t.Fatalf("callback returned %d, expected %d: %s", recorder.Code, want, recorder.Body.String())
		}
	}
	if calls.Load() != 1 || o.Pool.Get("rc-fixture-user") == nil {
		t.Fatalf("expected one exchange and one account, exchanges = %d", calls.Load())
	}
	poll := httptest.NewRecorder()
	o.raccoonLoginPoll(poll, httptest.NewRequest(http.MethodGet, "/?state=web", nil))
	if !strings.Contains(poll.Body.String(), `"status":"ok"`) {
		t.Fatalf("poll did not report success: %s", poll.Body.String())
	}
}

func TestRaccoonPastedAuthJSONNormalizesCredential(t *testing.T) {
	token := raccoonTestToken(t, "fixture-user")
	refresh := "fixture-refresh"
	document, err := json.Marshal(map[string]string{"access_token": token, "refresh_token": refresh})
	if err != nil {
		t.Fatal(err)
	}
	var invalidHeader atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			invalidHeader.Store(true)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprint(w, `{"code":0,"data":{"id":12345,"officeIdentity":"fixture-org"}}`)
	}))
	defer upstream.Close()
	t.Setenv("WB_RACCOON_AUTH_BASE", upstream.URL)
	pasted, _ := json.Marshal(map[string]string{"token": string(document)})
	for _, body := range [][]byte{document, pasted} {
		o := &Operations{Pool: pool.New(nil)}
		recorder := httptest.NewRecorder()
		o.raccoonImportAccount(recorder, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("import returned %d: %s", recorder.Code, recorder.Body.String())
		}
		acc := o.Pool.Get("rc-12345")
		if acc == nil || acc.Record().AccessToken != token || acc.Record().RefreshToken != refresh || acc.Record().RaccoonOrgCode() != "fixture-org" || invalidHeader.Load() {
			t.Fatal("import did not send and store normalized credentials and the organization")
		}
	}
}

func TestRaccoonLoginDoesNotPublishFailedPersistenceOrReplaceForeignUID(t *testing.T) {
	old := &accounts.Record{UID: "rc-fixture-user", Realm: realm.Raccoon, AccessToken: "old", Enabled: false}
	cred := raccoon.Credential{Token: raccoonTestToken(t, "fixture-user"), UserID: "fixture-user"}
	o := &Operations{Pool: pool.New([]*accounts.Record{old})}
	o.Persist = func(*accounts.Record) error { return errors.New("store unavailable") }
	if _, err := o.saveRaccoonCredential(cred, "", "raccoon-import"); !errors.Is(err, errRaccoonAccountSave) {
		t.Fatalf("expected save error, got %v", err)
	}
	if live := o.Pool.Get(old.UID); live.Record() != old || live.Enabled() {
		t.Fatal("failed save published the credential or enabled the account")
	}
	o.Persist = func(*accounts.Record) error {
		o.Pool.Update(old.UID, func(rec *accounts.Record) { rec.AccessToken = "newer-credential" })
		return nil
	}
	if _, err := o.saveRaccoonCredential(cred, "", "raccoon-import"); !errors.Is(err, errRaccoonAccountMoved) {
		t.Fatalf("expected generation conflict, got %v", err)
	}
	if live := o.Pool.Get(old.UID); live.Record().AccessToken != "newer-credential" || live.Enabled() {
		t.Fatal("a newer credential was overwritten")
	}
	foreign := &accounts.Record{UID: old.UID, Realm: realm.Intl, AccessToken: "other"}
	o.Pool = pool.New([]*accounts.Record{foreign})
	o.Persist = func(*accounts.Record) error { t.Fatal("foreign UID reached persistence"); return nil }
	if _, err := o.saveRaccoonCredential(cred, "", "raccoon-oauth"); !errors.Is(err, errRaccoonUIDConflict) {
		t.Fatalf("expected UID conflict, got %v", err)
	}
}

// TestChannelCatalogueNeverFallsBackToWorkBuddy proves the panel's model table
// for a non-WorkBuddy channel is built from that channel's accounts (it used to
// answer with the WorkBuddy catalogue whenever realm was not intl/cn).
func TestChannelCatalogueNeverFallsBackToWorkBuddy(t *testing.T) {
	rec := &accounts.Record{UID: "rc-1", Realm: realm.Raccoon, AccessToken: "t",
		Extra: map[string]any{raccoon.ExtraKey: map[string]any{
			"models":         []any{"raccoon-090dd3", "sn-glm-5-3"},
			"models_visible": []any{"sn-glm-5-3"},
			"models_at":      float64(1795000000),
			"models_meta": []any{map[string]any{
				"id": "sn-glm-5-3", "name": "GLM-5-3", "context_window": float64(1000000),
				"max_tokens": float64(100000), "supports_reasoning": true,
				"reasoning_levels": []any{"minimal", "medium", "max"}, "visible": true, "kind": "chat",
			}},
		}}}
	s := &Settings{Pool: pool.New([]*accounts.Record{rec})}
	models, fetched := s.channelCatalogue(realm.Raccoon)
	if len(models) != 1 {
		t.Fatalf("models = %d, want the single visible catalogue entry: %#v", len(models), models)
	}
	entry := models[0].(map[string]any)
	if entry["id"] != "sn-glm-5-3" {
		t.Fatalf("id = %v", entry["id"])
	}
	if entry["context_length"] != int64(1000000) {
		t.Fatalf("context_length = %v", entry["context_length"])
	}
	levels, _ := entry["reasoning_efforts"].([]string)
	if len(levels) != 3 || levels[0] != "minimal" {
		t.Fatalf("reasoning_efforts = %#v", entry["reasoning_efforts"])
	}
	if fetched != 1795000000 {
		t.Fatalf("fetched_at = %d", fetched)
	}
	// The hidden Raccoon-Work id must not appear in the panel table.
	for _, m := range models {
		if m.(map[string]any)["id"] == "raccoon-090dd3" {
			t.Fatal("hidden model leaked into the panel catalogue")
		}
	}
	// A CodeArts account answers from its declared ids, not from WorkBuddy.
	hw := &accounts.Record{UID: "hw-1", Realm: realm.CodeArts,
		Extra: map[string]any{codearts.ExtraKey: map[string]any{"models": []any{"GLM-5.2"}}}}
	s2 := &Settings{Pool: pool.New([]*accounts.Record{hw})}
	models, _ = s2.channelCatalogue(realm.CodeArts)
	if len(models) != 1 || models[0].(map[string]any)["id"] != "GLM-5.2" {
		t.Fatalf("codearts catalogue = %#v", models)
	}
}

// TestChannelCatalogueReadsNumbersFromDisk covers the shapes a record carries
// after a reload: the account store decodes with UseNumber, so every number in
// models_meta is a json.Number, not a float64.
func TestChannelCatalogueReadsNumbersFromDisk(t *testing.T) {
	hw := &accounts.Record{UID: "hw-1", Realm: realm.CodeArts,
		Extra: map[string]any{codearts.ExtraKey: map[string]any{
			"models": []any{"GLM-5.2"},
			"models_meta": []any{map[string]any{
				"id": "GLM-5.2", "name": "GLM-5.2",
				"context_window": json.Number("202752"),
				"max_tokens":     json.Number("65536"),
			}},
		}}}
	s := &Settings{Pool: pool.New([]*accounts.Record{hw})}
	models, _ := s.channelCatalogue(realm.CodeArts)
	entry := models[0].(map[string]any)
	if entry["context_length"] != int64(202752) {
		t.Fatalf("context_length = %#v", entry["context_length"])
	}
	if entry["max_output_tokens"] != int64(65536) {
		t.Fatalf("max_output_tokens = %#v", entry["max_output_tokens"])
	}
	// The raccoon metadata path reads the same shapes.
	rc := &accounts.Record{UID: "rc-1", Realm: realm.Raccoon,
		Extra: map[string]any{raccoon.ExtraKey: map[string]any{
			"models":         []any{"sn-glm-5-3"},
			"models_visible": []any{"sn-glm-5-3"},
			"models_meta": []any{map[string]any{
				"id": "sn-glm-5-3", "name": "GLM-5-3",
				"context_window": json.Number("1000000"),
				"max_tokens":     json.Number("100000"),
				"visible":        true,
			}},
		}}}
	meta := raccoon.MetaOfRecord(rc)
	if len(meta) != 1 || meta[0].ContextWindow != 1000000 || meta[0].MaxTokens != 100000 {
		t.Fatalf("raccoon meta = %#v", meta)
	}
}

// TestRaccoonCallbackInput covers what an operator can actually copy on a
// server deployment: the custom-scheme URL, an http callback that failed to
// load, or just the code.
func TestRaccoonCallbackInput(t *testing.T) {
	state := "st-1"
	cases := []struct{ name, raw, want string }{
		{"desktop scheme", "office-raccoon://auth/callback?code=C1&state=st-1", "C1"},
		{"http callback", "http://127.0.0.1:8788/api/v2/raccoon/login/callback?code=C2&state=st-1", "C2"},
		{"bare code", "C3", "C3"},
	}
	for _, c := range cases {
		got, err := raccoon.CodeFromInput(c.raw, state)
		if err != nil || got != c.want {
			t.Fatalf("%s: got %q err %v, want %q", c.name, got, err, c.want)
		}
	}
	// A callback whose state does not match is refused, not sent upstream.
	if _, err := raccoon.CodeFromInput("office-raccoon://auth/callback?code=C&state=other", state); err == nil {
		t.Fatal("mismatched state must be refused")
	}
	// Garbage that is not a code must not be forwarded as one.
	if _, err := raccoon.CodeFromInput("not a code at all", state); err == nil {
		t.Fatal("whitespace-separated text must be refused")
	}
	// A bare code still requires a known attempt.
	rec := httptest.NewRecorder()
	o := &Operations{}
	req := httptest.NewRequest(http.MethodPost, "/api/v2/raccoon/login/complete",
		strings.NewReader(`{"state":"unknown","url":"C3"}`))
	req.Header.Set("Content-Type", "application/json")
	o.raccoonLoginComplete(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "过期") {
		t.Fatalf("unknown attempt: status %d body %s", rec.Code, rec.Body.String())
	}
}
