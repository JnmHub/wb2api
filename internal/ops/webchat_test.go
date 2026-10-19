package ops

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDailyChatWebDrivesACP(t *testing.T) {
	e := newEnv(t, "intl", nil)
	if !e.c.Settings.DailyChatWeb() || e.c.Settings.DailyChatWebAttempts() != 3 || e.c.Settings.DailyChatWebTimeout() != 120 {
		t.Fatalf("defaults: web=%v attempts=%d timeout=%d", e.c.Settings.DailyChatWeb(), e.c.Settings.DailyChatWebAttempts(), e.c.Settings.DailyChatWebTimeout())
	}
	_ = e.c.Settings.SetDailyChatWebAttempts(1)

	var mu sync.Mutex
	var methods []string
	var prompts int
	acp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Acp-Connection-Id", "conn-1")
			io.WriteString(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hi"}}}}`+"\n\n")
		case http.MethodPost:
			raw, _ := io.ReadAll(r.Body)
			var msg struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(raw, &msg)
			mu.Lock()
			methods = append(methods, msg.Method)
			if msg.Method == "session/prompt" {
				prompts++
			}
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer acp.Close()

	e.vendor.h["/console/as/conversations/"] = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"code":0,"data":{"id":"conv-1"}}`)
	}
	e.vendor.h["/console/as/conversations/conv-1/session"] = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":0,"data":{"link":"`+acp.URL+`/acp","token":"tok-1","sessionId":"sess-1","cwd":"/workspace"}}`)
	}
	e.vendor.h["/console/as/conversations/conv-1"] = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":0,"data":{"status":"completed"}}`)
	}

	res := e.c.DailyChatWeb(context.Background(), e.acc())
	if !res.OK {
		t.Fatalf("turn failed: %+v", res)
	}
	if res.Extra["conversation"] != "conv-1" {
		t.Fatalf("extra: %+v", res.Extra)
	}
	mu.Lock()
	got := strings.Join(methods, ",")
	mu.Unlock()
	want := "initialize,session/load,session/set_model,session/prompt"
	if got != want {
		t.Fatalf("ACP requests = %q, want %q", got, want)
	}
	if prompts != 1 {
		t.Fatalf("prompt count %d", prompts)
	}
	// The conversation was created with the web-client shape.
	if e.vendor.called("POST /console/as/conversations/") != 1 {
		t.Fatal("conversation not created exactly once")
	}
}

func TestWebTurnRetriesSlowSession(t *testing.T) {
	e := newEnv(t, "intl", nil)
	_ = e.c.Settings.SetDailyChatWebAttempts(2)
	_ = e.c.Settings.SetDailyChatWebTimeout(30)
	acp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Acp-Connection-Id", "c")
			w.Header().Set("Content-Type", "text/event-stream")
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer acp.Close()
	e.vendor.h["/console/as/conversations/"] = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":0,"data":{"id":"conv-2"}}`)
	}
	first := true
	e.vendor.h["/console/as/conversations/conv-2/session"] = func(w http.ResponseWriter, r *http.Request) {
		if first {
			first = false
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		io.WriteString(w, `{"code":0,"data":{"link":"`+acp.URL+`/acp","token":"t","sessionId":"s"}}`)
	}
	e.vendor.h["/console/as/conversations/conv-2"] = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":{"status":"completed"}}`)
	}
	res := e.c.DailyChatWeb(context.Background(), e.acc())
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	if res.Extra["attempts"] != 2 {
		t.Fatalf("attempts: %+v", res.Extra)
	}
}

func TestWebTurnRejectsCN(t *testing.T) {
	e := newEnv(t, "cn", nil)
	res := e.c.DailyChatWeb(context.Background(), e.acc())
	if res.OK || !strings.Contains(res.Error, "international") {
		t.Fatalf("%+v", res)
	}
}

var _ = time.Second
