package openai

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"wb2api/internal/jsonx"
	"wb2api/internal/pool"
	"wb2api/internal/risk"
	"wb2api/internal/store/accounts"
	"wb2api/internal/store/settings"
	"wb2api/internal/upstream"
)

// rawUpstream records the exact bytes of every request and answers via fn.
type rawUpstream struct {
	ln   net.Listener
	mu   sync.Mutex
	reqs []rawReq
}

type rawReq struct {
	head  []string // request line + header lines, in wire order
	body  []byte
	bytes []byte
}

func (r rawReq) header(name string) string {
	for _, l := range r.head[1:] {
		if i := strings.Index(l, ":"); i > 0 && strings.EqualFold(l[:i], name) {
			return strings.TrimSpace(l[i+1:])
		}
	}
	return ""
}

func (r rawReq) names() []string {
	var out []string
	for _, l := range r.head[1:] {
		if i := strings.Index(l, ":"); i > 0 {
			out = append(out, l[:i])
		}
	}
	return out
}

func (r rawReq) jsonBody(t *testing.T) *jsonx.Obj {
	t.Helper()
	b := r.body
	if r.header("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("body is not gzip: %v", err)
		}
		b, _ = io.ReadAll(zr)
	}
	o, err := jsonx.UnmarshalObj(b)
	if err != nil {
		t.Fatalf("upstream got invalid JSON: %v\n%s", err, b)
	}
	return o
}

func newRawUpstream(t *testing.T, fn func(n int, req rawReq, c net.Conn)) *rawUpstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := &rawUpstream{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					var head []string
					clen := 0
					var raw bytes.Buffer
					for {
						line, err := br.ReadString('\n')
						if err != nil {
							return
						}
						raw.WriteString(line)
						line = strings.TrimRight(line, "\r\n")
						if line == "" {
							break
						}
						head = append(head, line)
						if strings.HasPrefix(strings.ToLower(line), "content-length:") {
							clen, _ = strconv.Atoi(strings.TrimSpace(line[len("content-length:"):]))
						}
					}
					body := make([]byte, clen)
					if _, err := io.ReadFull(br, body); err != nil {
						return
					}
					raw.Write(body)
					u.mu.Lock()
					u.reqs = append(u.reqs, rawReq{head: head, body: body, bytes: raw.Bytes()})
					n := len(u.reqs)
					req := u.reqs[n-1]
					u.mu.Unlock()
					fn(n, req, c)
				}
			}()
		}
	}()
	return u
}

func (u *rawUpstream) url() string { return "http://" + u.ln.Addr().String() }

func (u *rawUpstream) requests() []rawReq {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]rawReq(nil), u.reqs...)
}

func sseOK(c net.Conn) {
	body := "data: {\"id\":\"u\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"u\",\"choices\":[],\"usage\":{\"total_tokens\":3}}\n\ndata: [DONE]\n\n"
	fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
}

func status(c net.Conn, code int, body string) {
	fmt.Fprintf(c, "HTTP/1.1 %d X\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", code, len(body), body)
}

type fixture struct {
	h    *Handler
	srv  *httptest.Server
	up   *rawUpstream
	pool *pool.Pool
	dir  string

	persisted []*accounts.Record
	pmu       sync.Mutex
}

type accSpec map[string]any

func newFixture(t *testing.T, up *rawUpstream, specs ...accSpec) *fixture {
	t.Helper()
	dir := t.TempDir()
	exp := time.Now().Add(24 * time.Hour).Unix()
	var recs []*accounts.Record
	for i, s := range specs {
		m := map[string]any{"uid": fmt.Sprintf("acct-%d", i), "accessToken": fmt.Sprintf("tok-%d", i), "realm": "intl", "expiresAt": exp}
		for k, v := range s {
			m[k] = v
		}
		recs = append(recs, accounts.FromMap(m, "", time.Now()))
	}
	f := &fixture{up: up, pool: pool.New(recs), dir: dir}
	f.h = &Handler{
		Pool: f.pool, Up: upstream.NewManager(upstream.DefaultTimeouts()), Settings: settings.Open(dir),
		UpstreamOverride: up.url(), MaxAttempts: 5,
		Risk: &risk.Guard{Cfg: risk.Config{RotateMax: 0, WaveMin: 2, WaveWindow: time.Hour, Hold: 30 * time.Minute}},
		Persist: func(r *accounts.Record) error {
			f.pmu.Lock()
			f.persisted = append(f.persisted, r)
			f.pmu.Unlock()
			return nil
		},
	}
	f.srv = httptest.NewServer(f.h)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) post(t *testing.T, body string, hdr map[string]string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestOutboundRequestHasIdentityHeadersInLegacyOrder(t *testing.T) {
	up := newRawUpstream(t, func(n int, _ rawReq, c net.Conn) { sseOK(c) })
	f := newFixture(t, up, accSpec{})
	code, _, _ := f.post(t, `{"model":"glm-5.3","messages":[{"role":"developer","content":"be brief"},{"role":"user","content":"hi"}],"max_completion_tokens":"64"}`, nil)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	r := up.requests()[0]
	if !strings.HasPrefix(r.head[0], "POST /v2/chat/completions HTTP/1.1") {
		t.Fatalf("request line %q", r.head[0])
	}
	names := r.names()
	// Wire shape: Accept-Encoding, Content-Length, Host, then the identity set
	// in legacy order, then Connection.
	prefix := []string{"Accept-Encoding", "Content-Length", "Host", "Content-Type", "Accept", "X-Requested-With", "Origin", "Referer", "X-CodeBuddy-Request", "Accept-Language", "X-Agent-Intent"}
	for i, want := range prefix {
		if i >= len(names) || names[i] != want {
			t.Fatalf("header %d = %v, want %s\nall: %v", i, names[min(i, len(names)-1)], want, names)
		}
	}
	if names[len(names)-1] != "Connection" {
		t.Fatalf("last header %q", names[len(names)-1])
	}
	if r.header("Authorization") != "Bearer tok-0" || r.header("X-User-Id") != "acct-0" ||
		r.header("User-Agent") != "WorkBuddy/5.8.4 workbuddy-ai/5.8.4 CLI/2.161.0" || r.header("X-IDE-Type") != "WorkBuddy" {
		t.Fatalf("identity values wrong: %v", r.head)
	}
	if r.header("Content-Encoding") != "" {
		t.Fatal("the desktop identity does not gzip")
	}
	body := string(r.body)
	// Compact JSON, client key order kept, additions at the end.
	want := `{"model":"glm-5.3","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}],"max_tokens":64,"stream":true,"stream_options":{"include_usage":true}}`
	if !strings.HasPrefix(body, want[:len(want)-len(`,"max_tokens":64,"stream":true,"stream_options":{"include_usage":true}}`)]) {
		t.Fatalf("body prefix wrong: %s", body)
	}
	if !strings.Contains(body, `"max_tokens":64`) || !strings.Contains(body, `"stream":true,"stream_options":{"include_usage":true}}`) || strings.Contains(body, " ") && !strings.Contains(body, "be brief") {
		t.Fatalf("body: %s", body)
	}
	if strings.Contains(body, "max_completion_tokens") || strings.Contains(body, `"developer"`) {
		t.Fatalf("not normalised: %s", body)
	}
	if r.header("Content-Length") != strconv.Itoa(len(r.body)) {
		t.Fatalf("content-length %s vs %d", r.header("Content-Length"), len(r.body))
	}
}

func TestCLIIdentityGzipsAndAdjustsBody(t *testing.T) {
	up := newRawUpstream(t, func(n int, _ rawReq, c net.Conn) { sseOK(c) })
	f := newFixture(t, up, accSpec{"product": "cli"})
	code, _, _ := f.post(t, `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}],"metadata":{"a":1},"conversation_id":"c-1","previous_response_id":"p"}`, nil)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	r := up.requests()[0]
	if r.header("Content-Encoding") != "gzip" {
		t.Fatalf("CLI identity must gzip the body: %v", r.head)
	}
	if r.header("X-IDE-Type") != "CLI" || !strings.HasPrefix(r.header("User-Agent"), "CLI/") || r.header("x-stainless-lang") != "js" {
		t.Fatalf("CLI identity headers missing: %v", r.head)
	}
	if r.header("Accept") != "application/json" || r.header("Origin") != "" {
		t.Fatal("CLI identity has no browser headers")
	}
	b := r.jsonBody(t)
	for _, k := range []string{"metadata", "conversation_id", "previous_response_id"} {
		if b.Has(k) {
			t.Errorf("CLI body must not carry %s", k)
		}
	}
	if b.Get("cache_retention") != "long" {
		t.Errorf("cache_retention missing: %v", b.Keys())
	}
	// A session key from the client derives a stable X-Conversation-ID per account.
	if got := r.header("X-Conversation-ID"); len(got) != 36 {
		t.Errorf("conversation id %q", got)
	}
	f.post(t, `{"model":"glm-5.3","messages":[{"role":"user","content":"again"}],"conversation_id":"c-1"}`, nil)
	f.post(t, `{"model":"glm-5.3","messages":[{"role":"user","content":"again"}],"conversation_id":"c-2"}`, nil)
	rs := up.requests()
	// first request carried conversation_id "c-1" in the CLIENT body, which is
	// also the session key
	if rs[0].header("X-Conversation-ID") != rs[1].header("X-Conversation-ID") {
		t.Error("same session on the same account must keep the same X-Conversation-ID")
	}
	if rs[1].header("X-Conversation-ID") == rs[2].header("X-Conversation-ID") {
		t.Error("different sessions must differ")
	}
	if rs[0].header("X-Conversation-Request-ID") == rs[1].header("X-Conversation-Request-ID") {
		t.Error("each user turn gets its own X-Conversation-Request-ID")
	}
}

func TestRetriesKeepTheSameTurnRequestID(t *testing.T) {
	up := newRawUpstream(t, func(n int, _ rawReq, c net.Conn) {
		if n == 1 {
			status(c, 429, `{"code":6004}`)
			return
		}
		sseOK(c)
	})
	f := newFixture(t, up, accSpec{"product": "cli"}, accSpec{"product": "cli"})
	if code, _, _ := f.post(t, chatBody, nil); code != 200 {
		t.Fatalf("status %d", code)
	}
	rs := up.requests()
	if len(rs) != 2 {
		t.Fatalf("expected a failover, got %d requests", len(rs))
	}
	if rs[0].header("X-Conversation-Request-ID") == "" || rs[0].header("X-Conversation-Request-ID") != rs[1].header("X-Conversation-Request-ID") {
		t.Error("a retried turn must reuse X-Conversation-Request-ID")
	}
	if rs[0].header("X-Request-ID") == rs[1].header("X-Request-ID") {
		t.Error("the message id is new for every attempt")
	}
}

type fakeRefresher struct {
	calls int
	err   error
	fn    func(a *pool.Account)
}

func (r *fakeRefresher) Refresh(ctx context.Context, a *pool.Account) error {
	r.calls++
	if r.err == nil && r.fn != nil {
		r.fn(a)
	}
	return r.err
}

func TestUnauthorizedRefreshesThenRetriesTheSameAccount(t *testing.T) {
	up := newRawUpstream(t, func(n int, req rawReq, c net.Conn) {
		if req.header("Authorization") == "Bearer tok-0" {
			status(c, 401, `{"msg":"expired"}`)
			return
		}
		sseOK(c)
	})
	f := newFixture(t, up, accSpec{}, accSpec{})
	fr := &fakeRefresher{fn: func(a *pool.Account) {
		rec := *a.Record()
		rec.AccessToken = "tok-new"
		a.SetRecord(&rec)
	}}
	f.h.Refresher = fr
	code, body, _ := f.post(t, chatBody, nil)
	if code != 200 || !strings.Contains(body, "hi") {
		t.Fatalf("status %d %s", code, body)
	}
	rs := up.requests()
	if len(rs) != 2 || rs[1].header("Authorization") != "Bearer tok-new" {
		t.Fatalf("expected a retry with the refreshed token on the same account, got %d requests: %v", len(rs), rs[len(rs)-1].head)
	}
	if fr.calls != 1 {
		t.Fatalf("refresh called %d times", fr.calls)
	}
}

func TestUnauthorizedWithFailedRefreshRotates(t *testing.T) {
	up := newRawUpstream(t, func(n int, req rawReq, c net.Conn) {
		if req.header("Authorization") == "Bearer tok-0" {
			status(c, 401, `{}`)
			return
		}
		sseOK(c)
	})
	f := newFixture(t, up, accSpec{}, accSpec{})
	f.h.Refresher = &fakeRefresher{err: fmt.Errorf("refresh down")}
	if code, _, _ := f.post(t, chatBody, nil); code != 200 {
		t.Fatalf("status %d", code)
	}
	rs := up.requests()
	if len(rs) != 2 || rs[1].header("Authorization") != "Bearer tok-1" {
		t.Fatalf("expected rotation to the other account: %d requests", len(rs))
	}
	if f.pool.All()[0].Ready("glm-5.3", time.Now()) {
		t.Error("the failing account should be cooling")
	}
}

const restricted403 = `{"code":11140,"msg":"request illegal","displayMsg":{"en":"Your account has been restricted due to unusual activity"}}`

func TestRestrictedAccountStopsWithoutRotatingAndPersists(t *testing.T) {
	up := newRawUpstream(t, func(n int, _ rawReq, c net.Conn) { status(c, 403, restricted403) })
	f := newFixture(t, up, accSpec{}, accSpec{}, accSpec{})
	code, body, _ := f.post(t, chatBody, nil)
	if code != 403 || !strings.Contains(body, "11140") {
		t.Fatalf("status %d %s", code, body)
	}
	if n := len(up.requests()); n != 1 {
		t.Fatalf("a restriction must not be replayed on other accounts; upstream saw %d requests", n)
	}
	f.pmu.Lock()
	defer f.pmu.Unlock()
	if len(f.persisted) != 1 || f.persisted[0].RestrictedUntil < float64(time.Now().Add(time.Hour).Unix()) {
		t.Fatalf("restriction not persisted for ~2h: %+v", f.persisted)
	}
	restricted := 0
	for _, a := range f.pool.All() {
		if !a.RestrictedUntil().IsZero() {
			restricted++
		}
	}
	if restricted != 1 {
		t.Fatalf("exactly one account should be parked, got %d", restricted)
	}
}

func TestRestrictionWavePausesTheRealm(t *testing.T) {
	up := newRawUpstream(t, func(n int, _ rawReq, c net.Conn) { status(c, 403, restricted403) })
	f := newFixture(t, up, accSpec{}, accSpec{}, accSpec{})
	f.post(t, chatBody, nil) // first account restricted
	f.post(t, chatBody, nil) // second account restricted -> wave
	n := len(up.requests())
	code, body, hdr := f.post(t, chatBody, nil)
	if code != http.StatusTooManyRequests || hdr.Get("Retry-After") == "" || !strings.Contains(body, "暂停派发") {
		t.Fatalf("realm should be paused: %d %s %v", code, body, hdr)
	}
	if len(up.requests()) != n {
		t.Fatal("while paused, no request may reach the upstream")
	}
}

func TestContentReviewRetriesOnceWithNeutralPrompt(t *testing.T) {
	up := newRawUpstream(t, func(n int, _ rawReq, c net.Conn) {
		if n == 1 {
			status(c, 403, `{"code":11128,"msg":"illegal"}`)
			return
		}
		sseOK(c)
	})
	f := newFixture(t, up, accSpec{})
	code, _, _ := f.post(t, `{"model":"glm-5.3","messages":[{"role":"system","content":"client system"},{"role":"user","content":"hi"}]}`, nil)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	rs := up.requests()
	if len(rs) != 2 {
		t.Fatalf("want one degraded retry, got %d requests", len(rs))
	}
	first := rs[0].jsonBody(t).Get("messages").([]any)[0].(*jsonx.Obj).Get("content")
	second := rs[1].jsonBody(t).Get("messages").([]any)[0].(*jsonx.Obj).Get("content")
	if first != "client system" || !strings.HasPrefix(second.(string), "You are a helpful assistant.") {
		t.Fatalf("system prompt not degraded on retry: %q -> %q", first, second)
	}
	// The degrade window now applies to following requests too.
	f.post(t, `{"model":"glm-5.3","messages":[{"role":"system","content":"client system"},{"role":"user","content":"x"}]}`, nil)
	third := up.requests()[2].jsonBody(t).Get("messages").([]any)[0].(*jsonx.Obj).Get("content")
	if !strings.HasPrefix(third.(string), "You are a helpful assistant.") {
		t.Fatalf("degrade window should persist: %q", third)
	}
}

func TestContentReviewTwiceIsPassedThrough(t *testing.T) {
	up := newRawUpstream(t, func(n int, _ rawReq, c net.Conn) { status(c, 403, `{"code":11128}`) })
	f := newFixture(t, up, accSpec{}, accSpec{})
	code, body, _ := f.post(t, chatBody, nil)
	if code != 403 || !strings.Contains(body, "upstream 403") {
		t.Fatalf("status %d %s", code, body)
	}
	if n := len(up.requests()); n != 2 {
		t.Fatalf("expected the original attempt plus one degraded retry, got %d", n)
	}
}

func TestPromptFileCustomMode(t *testing.T) {
	up := newRawUpstream(t, func(n int, _ rawReq, c net.Conn) { sseOK(c) })
	f := newFixture(t, up, accSpec{})
	pf := filepath.Join(f.dir, "prompt.txt")
	if err := os.WriteFile(pf, []byte("OPERATOR PROMPT"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`{"prompt":{"mode":"custom","file":%q}}`, filepath.ToSlash(pf))
	if err := os.WriteFile(filepath.Join(f.dir, "settings.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(settings.CheckInterval + 100*time.Millisecond)
	f.post(t, `{"model":"glm-5.3","messages":[{"role":"system","content":"client"},{"role":"user","content":"hi"}]}`, nil)
	msgs := up.requests()[0].jsonBody(t).Get("messages").([]any)
	if len(msgs) != 2 || msgs[0].(*jsonx.Obj).Get("content") != "OPERATOR PROMPT" {
		t.Fatalf("custom prompt not applied: %v", msgs)
	}
}

func TestUpstreamTimeoutsComeFromSettings(t *testing.T) {
	up := newRawUpstream(t, func(n int, _ rawReq, c net.Conn) { time.Sleep(3 * time.Second) })
	f := newFixture(t, up, accSpec{})
	f.h.MaxAttempts = 1 // a timeout is retried like any transient failure; measure a single attempt
	if err := os.WriteFile(filepath.Join(f.dir, "settings.json"), []byte(`{"upstream":{"header_timeout_seconds":1}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(settings.CheckInterval + 100*time.Millisecond)
	start := time.Now()
	code, _, _ := f.post(t, chatBody, nil)
	if code == 200 || time.Since(start) > 2500*time.Millisecond {
		t.Fatalf("header timeout of 1 s not applied: status %d after %v", code, time.Since(start))
	}
}
