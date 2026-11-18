package codearts

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHeartbeatProtocol(t *testing.T) {
	var gotPath, gotStatus, gotSession, gotClient, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotStatus = r.URL.Query().Get("status")
		gotSession = r.Header.Get("user-session-id")
		gotClient = r.Header.Get("x-client-type")
		gotAuth = r.Header.Get("Authorization")
		buf := make([]byte, 8)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		if r.Method != http.MethodPut {
			t.Errorf("method = %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`)
	}))
	defer srv.Close()

	cred := Credential{AccessKeyID: "AK", SecretAccessKey: "SK", SecurityToken: "STS"}
	if err := Heartbeat(context.Background(), srv.Client(), srv.URL, cred, "abc123", SessionBusy); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if gotPath != HeartbeatPath || gotStatus != SessionBusy {
		t.Fatalf("path=%s status=%s", gotPath, gotStatus)
	}
	if gotSession != "abc123" || gotClient != "kernel" {
		t.Fatalf("session=%s client=%s", gotSession, gotClient)
	}
	if !strings.HasPrefix(gotAuth, "SDK-HMAC-SHA256 Access=AK, ") {
		t.Fatalf("unsigned: %q", gotAuth)
	}
	if strings.TrimSpace(gotBody) != "{}" {
		t.Fatalf("body = %q", gotBody)
	}
}

func TestHeartbeatRequiresConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"not-ok", `{"status":"no"}`, 200},
		{"empty", `{}`, 200},
		{"http-400", `{"error_code":"TM.00001041"}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			err := Heartbeat(context.Background(), srv.Client(), srv.URL, Credential{AccessKeyID: "AK", SecretAccessKey: "SK"}, "id", SessionBusy)
			if err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestNewUserSessionIDShape(t *testing.T) {
	a, err := NewUserSessionID()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewUserSessionID()
	if len(a) != 32 || a == b {
		t.Fatalf("ids = %q / %q", a, b)
	}
	for _, c := range a {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("id is not hex: %q", a)
		}
	}
}

// The chat request must carry the same id that was registered busy.
func TestChatHeadersCarriesSessionID(t *testing.T) {
	h := ChatHeaders(Credential{AccessKeyID: "AK", SecretAccessKey: "SK", SecurityToken: "T"}, "T", "trace", "", "sess-1", "GLM-5.2", false, []byte(`{}`))
	if v, _ := h.Get("user-session-id"); v != "sess-1" {
		t.Fatalf("user-session-id = %q", v)
	}
	auth, _ := h.Get("Authorization")
	if !strings.Contains(auth, "user-session-id") {
		t.Fatalf("the session header must be signed: %s", auth)
	}
	// Without a lease the header is absent (probe/backfill paths).
	h2 := ChatHeaders(Credential{AccessKeyID: "AK", SecretAccessKey: "SK"}, "T", "trace", "", "", "GLM-5.2", false, []byte(`{}`))
	if _, ok := h2.Get("user-session-id"); ok {
		t.Fatal("empty session id must not be sent")
	}
}

func TestIsSessionLimitError(t *testing.T) {
	if !IsSessionLimitError([]byte(`{"error_code":"TM.00001041","error_msg":"session limit"}`)) {
		t.Fatal("TM.00001041 not recognised")
	}
	if IsSessionLimitError([]byte(`{"error_code":"InferHub.001001005"}`)) {
		t.Fatal("false positive")
	}
}

var _ atomic.Int32

// The upstream treats the model name as a request header: three keys, all
// carrying the wire name (catalogue id), not the display alias.
func TestChatHeadersCarryModelTriplet(t *testing.T) {
	h := ChatHeaders(Credential{AccessKeyID: "AK", SecretAccessKey: "SK", SecurityToken: "T"},
		"T", "trace", "", "", "deepseek-v4.1-flash", false, []byte(`{}`))
	for _, key := range []string{"model-id", "model-name", "x-model-id"} {
		if v, ok := h.Get(key); !ok || v != "deepseek-v4.1-flash" {
			t.Fatalf("%s = %q (present %v)", key, v, ok)
		}
	}
	auth, _ := h.Get("Authorization")
	for _, key := range []string{"model-id", "model-name", "x-model-id", "agent-type", "plugin-name", "plugin-version"} {
		if !strings.Contains(auth, key) {
			t.Fatalf("%s is missing from SignedHeaders: %s", key, auth)
		}
	}
}

// APIG/TM refusals are about the account or the gateway, not the request body.
func TestIsGatewayRejection(t *testing.T) {
	yes := []string{
		`{"error_code":"TM.00001050","error_msg":"invalid request parameters"}`,
		`{"error_code":"TM.00001041","error_msg":"并发会话数已达上限"}`,
		`{"error_code":"APIG.0301","error_msg":"decrypt token fail"}`,
		`{"error_code":"APIG.0602","error_msg":"security token has expired"}`,
	}
	for _, body := range yes {
		if !IsGatewayRejection([]byte(body)) {
			t.Errorf("not recognised: %s", body)
		}
	}
	for _, body := range []string{
		`{"error_code":"InferHub.001001005.400"}`,
		`{"error_code":"TM.00001051"}`,
		`{"error_code":"InferHub.001001009.400"}`,
	} {
		if IsGatewayRejection([]byte(body)) {
			t.Errorf("false positive: %s", body)
		}
	}
}
