package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wb2api/internal/agents"
)

func TestAgentsEndpoints(t *testing.T) {
	home := t.TempDir()
	mgr := agents.New(t.TempDir())
	mgr.Home = home
	mux := http.NewServeMux()
	(&Operations{Agents: mgr}).Register(mux, func(h http.Handler) http.Handler { return h })

	do := func(method, path, body, remote string) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	code, body := do("GET", "/api/v2/agents", "", "127.0.0.1:5555")
	if code != 200 || !strings.Contains(body, "claude-code") || !strings.Contains(body, `"loopback":true`) {
		t.Fatalf("list: %d %s", code, body)
	}
	code, body = do("POST", "/api/v2/agents/apply", `{"client":"claude-code","base_url":"http://127.0.0.1:8787","api_key":"sk-a","model":"m1"}`, "192.0.2.10:5555")
	if code != 403 {
		t.Fatalf("non-loopback apply must be 403: %d %s", code, body)
	}
	code, body = do("POST", "/api/v2/agents/apply", `{"client":"claude-code","base_url":"http://127.0.0.1:8787","api_key":"sk-a","model":"m1"}`, "127.0.0.1:5555")
	if code != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("apply: %d %s", code, body)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil || !strings.Contains(string(raw), "sk-a") {
		t.Fatalf("written: %v %s", err, raw)
	}
	code, body = do("POST", "/api/v2/agents/restore", `{"client":"claude-code"}`, "127.0.0.1:5555")
	if code != 200 || (!strings.Contains(body, `"restored"`) && !strings.Contains(body, `"deleted"`)) {
		t.Fatalf("restore: %d %s", code, body)
	}
	var list struct {
		Clients []agents.ClientView `json:"clients"`
	}
	_, body = do("GET", "/api/v2/agents", "", "127.0.0.1:5555")
	_ = json.Unmarshal([]byte(body), &list)
	found := false
	for _, c := range list.Clients {
		if c.ID == "claude-code" {
			found = true
			if c.Configured {
				t.Fatal("restore must clear the integration record")
			}
		}
	}
	if !found {
		t.Fatal("claude-code missing from the list")
	}
}
