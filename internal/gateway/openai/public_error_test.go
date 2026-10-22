package openai

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wb2api/internal/pool"
)

const privateErrorText = "Bearer private-token; https://proxy-user:proxy-password@private.host:1234/path; private-prompt; 10.23.45.67"

func assertNoPrivateError(t *testing.T, text string) {
	t.Helper()
	for _, forbidden := range []string{"private-token", "proxy-user", "proxy-password", "private.host", "private-prompt", "10.23.45.67"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("public error contains %q: %s", forbidden, text)
		}
	}
}

func TestPublicUpstreamDetailKeepsSafeClassification(t *testing.T) {
	for _, tc := range []struct {
		status     int
		body, want string
	}{
		{403, `{"code":11140,"msg":"` + privateErrorText + `"}`, "11140"},
		{400, `{"code":11133,"msg":"` + privateErrorText + `"}`, "invalid request parameters"},
		{400, `{"code":11135,"msg":"` + privateErrorText + `"}`, "replace the image"},
		{500, `{"code":11134,"msg":"` + privateErrorText + `"}`, "11134"},
		{429, `{"code":6004,"msg":"reset at 2030-01-02 12:00:00 UTC+8 ` + privateErrorText + `"}`, "reset at 2030-01-02T04:00:00Z"},
		{429, "account-level rate limit " + privateErrorText, "account rate limit"},
		{400, "context length exceeded " + privateErrorText, "maximum context length"},
		{403, "WAF " + privateErrorText, "WAF"},
		{500, privateErrorText, "upstream service unavailable"},
		{418, privateErrorText, "upstream rejected"},
	} {
		t.Run(fmt.Sprint(tc.status)+"/"+tc.want, func(t *testing.T) {
			got := publicUpstreamDetail(tc.status, []byte(tc.body))
			assertNoPrivateError(t, got)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestInitialHTTPErrorSanitizedForAllProtocols(t *testing.T) {
	t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
	for _, protocol := range []string{"chat", "responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(400)
				fmt.Fprintf(w, `{"code":11133,"msg":%q}`, privateErrorText)
			}), 1, "")
			var handler http.Handler = e.h
			body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
			if protocol == "responses" {
				handler = &Responses{Chat: e.h}
				body = `{"model":"m","input":"hi"}`
			} else if protocol == "messages" {
				handler = &Messages{Chat: e.h}
				body = `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
			}
			srv := httptest.NewServer(handler)
			defer srv.Close()
			resp, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			assertNoPrivateError(t, string(b))
			if resp.StatusCode != 400 || !strings.Contains(string(b), "11133") {
				t.Fatalf("status=%d body=%s", resp.StatusCode, b)
			}
		})
	}
}

func TestFinalFailureErrorsSanitized(t *testing.T) {
	for _, anth := range []bool{false, true} {
		for _, tc := range []struct {
			name   string
			state  attemptState
			status int
		}{
			{"tls", attemptState{kind: failTLS, lastErr: errors.New(privateErrorText)}, 502},
			{"network", attemptState{kind: failLocalNet, lastErr: errors.New(privateErrorText)}, 503},
			{"transport", attemptState{kind: failTransport, lastErr: errors.New(privateErrorText)}, 502},
			{"http", attemptState{kind: failHTTP, lastStatus: 500, lastBody: []byte(privateErrorText)}, 500},
			{"forbidden", attemptState{kind: failHTTP, last403Detail: privateErrorText}, 403},
			{"rate", attemptState{kind: failHTTP, last429: true, last429Detail: privateErrorText}, 429},
		} {
			t.Run(fmt.Sprint(anth)+"/"+tc.name, func(t *testing.T) {
				w := httptest.NewRecorder()
				h := &Handler{Pool: pool.New(nil)}
				q := &request{r: httptest.NewRequest(http.MethodPost, "/", nil), w: w, start: time.Now(), anth: anth, at: tc.state}
				h.fail(q)
				assertNoPrivateError(t, w.Body.String())
				if w.Code != tc.status {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				if tc.status == 429 && w.Header().Get("Retry-After") == "" {
					t.Fatal("missing Retry-After")
				}
				if anth && !strings.Contains(w.Body.String(), `"type":"error"`) {
					t.Fatal("lost Anthropic envelope")
				}
			})
		}
	}
}
