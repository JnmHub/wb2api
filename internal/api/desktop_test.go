package api

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wb2api/internal/activity"
	"wb2api/internal/atrest"
	"wb2api/internal/ops"
	"wb2api/internal/pool"
	"wb2api/internal/store/settings"
	"wb2api/internal/upstream"
)

func jwtFor(sub, iss string, exp int64) string {
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	return enc(map[string]any{"alg": "none"}) + "." + enc(map[string]any{"sub": sub, "iss": iss, "exp": exp}) + ".sig"
}

func TestDesktopCredentialImport(t *testing.T) {
	dir := t.TempDir()
	old := os.Getenv("LOCALAPPDATA")
	_ = os.Setenv("LOCALAPPDATA", dir)
	defer func() { _ = os.Setenv("LOCALAPPDATA", old) }()

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(7 + i)
	}
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(50 + i)
	}
	wrapped, err := atrest.Seal(key, master, "file")
	if err != nil {
		t.Fatal(err)
	}
	blob := map[string]any{"keyId": atrest.KeyID(master), "slots": []any{map[string]any{
		"type": "static-v1", "protectorKeyId": atrest.KeyID(key), "wrapped": wrapped,
	}}}
	blobPath := filepath.Join(dir, "keyblob")
	b, _ := json.Marshal(blob)
	if err := os.WriteFile(blobPath, b, 0o600); err != nil {
		t.Fatal(err)
	}

	token := jwtFor("uid-desktop", "https://www.codebuddy.cn", time.Now().Add(48*time.Hour).Unix())
	tokenEnv, _ := atrest.Seal(key, []byte(token), "field")
	authDir := filepath.Join(dir, "CodeBuddyExtension", "Data", "Public", "auth")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{
		"auth":    map[string]any{"accessToken": tokenEnv, "expiresAt": time.Now().Add(48 * time.Hour).Unix()},
		"account": map[string]any{"uid": "uid-desktop", "nickname": "Desktop User"},
	}
	raw, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(authDir, "workbuddy-desktop.info"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":0,"msg":"ok"}`)
	}))
	defer vendor.Close()

	p := pool.New(nil)
	client := &ops.Client{Pool: p, Up: upstream.NewManager(upstream.DefaultTimeouts()), Settings: settings.Open(dir),
		Activity: &activity.Log{Dir: dir}, BaseOverride: vendor.URL}
	keyring := &atrest.Keyring{BlobPath: blobPath}
	mux := http.NewServeMux()
	(&Operations{Ops: client, Pool: p, AtRest: keyring}).Register(mux, func(h http.Handler) http.Handler { return h })

	post := func(body string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v2/accounts/desktop", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	body := post(`{"keyText":"` + hex.EncodeToString(key) + `"}`)
	if !strings.Contains(body, `"key_cached":true`) {
		t.Fatalf("key not adopted: %s", body)
	}
	body = post(`{"all":true}`)
	if !strings.Contains(body, `"ok":true`) || !strings.Contains(body, `"uid":"uid-desktop"`) {
		t.Fatalf("import: %s", body)
	}
	if p.Get("uid-desktop") == nil {
		t.Fatal("the account must be in the pool")
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v2/accounts/desktop", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "workbuddy-desktop.info") {
		t.Fatalf("scan: %d %s", rec.Code, rec.Body.String())
	}
}
