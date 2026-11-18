package codearts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wb2api/internal/realm"
	"wb2api/internal/upstream/identity"
)

// fixedClock pins X-Sdk-Date so the signature vector below is reproducible.
func fixedClock() time.Time { return time.Date(2026, 11, 17, 8, 30, 0, 0, time.UTC) }

// baseHeaders is the unsigned header set of one chat attempt.
func baseHeaders() identity.Headers {
	return identity.Headers{
		identity.P("Content-Type", "application/json"),
		identity.P("Accept", "text/event-stream"),
		identity.P("x-auth-token", "TOKEN"),
		identity.P("x-snap-traceid", "trace-1"),
		identity.P("X-Language", "zh-cn"),
		identity.P("app-id", "CodeAgent3.0"),
		identity.P("is_confidential", "false"),
	}
}

// TestSignerKnownVector pins the whole canonicalisation: the expected value was
// produced by an independent implementation of Huawei's SDK-HMAC-SHA256
// (Python, from the SDK spec) over exactly these inputs. A change in the
// trailing-slash rule, the header casing or the signed-header ordering breaks
// this test instead of failing at the vendor with an opaque 401.
func TestSignerKnownVector(t *testing.T) {
	const (
		wantPayload = "2637e7152a30ada9cf431759a4453b36cd120a751f06674c4807dc00588a57bd"
		wantAuth    = "SDK-HMAC-SHA256 Access=AKIDEXAMPLE, " +
			"SignedHeaders=accept;app-id;content-type;host;is_confidential;x-auth-token;x-language;x-sdk-content-sha256;x-sdk-date;x-security-token;x-snap-traceid, " +
			"Signature=915adee3783fba7cc043b2aeba396bd897691d06289322d075aa6e00d9d4b2ac"
	)
	body := []byte(`{"model":"GLM-5.2","stream":true}`)
	s := Signer{Now: fixedClock}
	out := s.SignHeaders(baseHeaders(), http.MethodPost, realm.CodeArtsChatBase+ChatPath, body,
		Credential{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "SECRETEXAMPLEKEY", SecurityToken: "SECURITY_TOKEN_EXAMPLE"})

	if got, _ := out.Get("Authorization"); got != wantAuth {
		t.Fatalf("Authorization mismatch\n got: %s\nwant: %s", got, wantAuth)
	}
	if got, _ := out.Get("X-Sdk-Content-Sha256"); got != wantPayload {
		t.Fatalf("payload hash = %s, want %s", got, wantPayload)
	}
	if got, _ := out.Get("X-Sdk-Date"); got != "20261117T083000Z" {
		t.Fatalf("X-Sdk-Date = %s", got)
	}
	if got, _ := out.Get("Host"); got != "snap-access.cn-north-4.myhuaweicloud.com" {
		t.Fatalf("Host = %s", got)
	}
}

func TestSignerCoversTheBody(t *testing.T) {
	s := Signer{Now: fixedClock}
	a := s.SignHeaders(baseHeaders(), http.MethodPost, realm.CodeArtsChatBase+ChatPath, []byte(`{"a":1}`), Credential{AccessKeyID: "AK", SecretAccessKey: "SK"})
	b := s.SignHeaders(baseHeaders(), http.MethodPost, realm.CodeArtsChatBase+ChatPath, []byte(`{"a":2}`), Credential{AccessKeyID: "AK", SecretAccessKey: "SK"})
	ah, _ := a.Get("Authorization")
	bh, _ := b.Get("Authorization")
	if ah == bh {
		t.Fatal("signature does not depend on the payload")
	}
}

func TestSignerWithoutSecurityTokenOmitsTheHeader(t *testing.T) {
	s := Signer{Now: fixedClock}
	out := s.SignHeaders(baseHeaders(), http.MethodPost, realm.CodeArtsChatBase+ChatPath, nil, Credential{AccessKeyID: "AK", SecretAccessKey: "SK"})
	if _, ok := out.Get("X-Security-Token"); ok {
		t.Fatal("X-Security-Token must not be signed without a token")
	}
	auth, _ := out.Get("Authorization")
	if strings.Contains(auth, "x-security-token") {
		t.Fatal("unsigned header leaked into SignedHeaders")
	}
}

func TestCanonicalURICarriesTrailingSlash(t *testing.T) {
	if got := canonicalURI("/api/v2/chat/completions"); got != "/api/v2/chat/completions/" {
		t.Fatalf("canonicalURI = %q", got)
	}
	if got := canonicalURI("/api/v2/chat/completions/"); got != "/api/v2/chat/completions/" {
		t.Fatalf("canonicalURI = %q (must not double the slash)", got)
	}
}

func TestCanonicalQuerySorts(t *testing.T) {
	if got := canonicalQuery("b=2&a=1&a=0"); got != "a=0&a=1&b=2" {
		t.Fatalf("canonicalQuery = %q", got)
	}
	if got := canonicalQuery(""); got != "" {
		t.Fatalf("canonicalQuery empty = %q", got)
	}
}

// PrepareBody must reduce the WorkBuddy-shaped body to what the endpoint
// accepts: the upstream answers unknown fields with InferHub.001001005.
func TestPrepareBodyDropsWorkBuddyFields(t *testing.T) {
	shaped := newObj(t, `{
		"model":"claude-sonnet-4",
		"messages":[{"role":"user","content":"hi"}],
		"stream":false,
		"metadata":{"product":"workbuddy"},
		"conversation_id":"conv-1",
		"previous_response_id":"resp-1",
		"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],
		"temperature":0.2
	}`)
	body, _, err := PrepareBody(shaped, "GLM-5.2", "chat-42")
	if err != nil {
		t.Fatalf("PrepareBody: %v", err)
	}
	got := parseObj(t, body)
	if v, _ := got.Get("model").(string); v != "GLM-5.2" {
		t.Fatalf("model = %v", got.Get("model"))
	}
	if v, _ := got.Get("stream").(bool); !v {
		t.Fatal("stream must be forced to true: the upstream is stream-only on the wire")
	}
	if v, _ := got.Get("tool_stream").(bool); !v {
		t.Fatal("tool_stream must be set")
	}
	for _, dropped := range []string{"metadata", "conversation_id", "previous_response_id"} {
		if got.Has(dropped) {
			t.Fatalf("%s must not be forwarded", dropped)
		}
	}
	if !got.Has("tools") || !got.Has("temperature") || !got.Has("messages") {
		t.Fatalf("supported fields were dropped: %v", got.Keys())
	}
	if v, _ := got.Get("chat_id").(string); v != "chat-42" {
		t.Fatalf("chat_id = %v", got.Get("chat_id"))
	}
	if v, _ := got.Get("prompt_cache_key").(string); v != "chat-42" {
		t.Fatalf("prompt_cache_key = %v", got.Get("prompt_cache_key"))
	}
}

func TestPrepareBodyWithoutChatIDOmitsAffinityFields(t *testing.T) {
	body, _, err := PrepareBody(newObj(t, `{"messages":[]}`), "GLM-5.2", "")
	if err != nil {
		t.Fatal(err)
	}
	got := parseObj(t, body)
	if got.Has("chat_id") || got.Has("prompt_cache_key") {
		t.Fatal("no chat id means no affinity fields")
	}
}

// The benefit route header is part of the signature: it must be present before
// signing, not appended afterwards.
func TestChatHeadersSignsBenefitRoute(t *testing.T) {
	body := []byte(`{"model":"GLM-5.2","stream":true}`)
	plain := ChatHeaders(Credential{AccessKeyID: "AK", SecretAccessKey: "SK", SecurityToken: "T"}, "T", "trace", "", "", false, body)
	benefit := ChatHeaders(Credential{AccessKeyID: "AK", SecretAccessKey: "SK", SecurityToken: "T"}, "T", "trace", "", "", true, body)

	if _, ok := plain.Get(HeaderMaasType); ok {
		t.Fatal("maas_type must not be sent for a normal model")
	}
	if v, ok := benefit.Get(HeaderMaasType); !ok || v != MaasBenefit {
		t.Fatalf("maas_type = %v", v)
	}
	bh, _ := benefit.Get("Authorization")
	if !strings.Contains(bh, "maas_type") {
		t.Fatal("maas_type is missing from SignedHeaders: the upstream would reject the signature")
	}
	ph, _ := plain.Get("Authorization")
	if ph == bh {
		t.Fatal("benefit and normal signatures must differ")
	}
	if v, _ := benefit.Get("app-id"); v != "CodeAgent3.0" {
		t.Fatalf("app-id = %q", v)
	}
	if v, _ := benefit.Get("x-auth-token"); v != "T" {
		t.Fatalf("x-auth-token = %q", v)
	}
}

func TestChatHeadersSignsTheBytesOnTheWire(t *testing.T) {
	body := []byte(`{"model":"GLM-5.2","stream":true}`)
	h := ChatHeaders(Credential{AccessKeyID: "AK", SecretAccessKey: "SK", SecurityToken: "T"}, "T", "trace", "", "", false, body)
	want := sha256.Sum256(body)
	if got, _ := h.Get("X-Sdk-Content-Sha256"); got != hex.EncodeToString(want[:]) {
		t.Fatalf("payload hash does not cover the exact wire body: %s", got)
	}
}

func TestErrorClassification(t *testing.T) {
	if !IsFatalRequestError([]byte(`{"error_code":"InferHub.001001005","error_msg":"invalid parameter"}`)) {
		t.Fatal("parameter rejection must be fatal (no rotation, no cooldown)")
	}
	if IsFatalRequestError([]byte(`{"error_code":"InferHub.002002009.404"}`)) {
		t.Fatal("a missing model registration is not a request-shape error")
	}
	for _, body := range []string{
		`{"error_code":"InferHub.002002009.404","error_msg":"model not registered"}`,
		`{"code":"TM.000010051","msg":"无权调用该模型"}`,
		`{"error_msg":"InferHub.4004.200 benefit not found"}`,
	} {
		if !IsModelAccessError([]byte(body)) {
			t.Fatalf("not classified as a model access error: %s", body)
		}
	}
	if IsModelAccessError([]byte(`{"error":"boom"}`)) {
		t.Fatal("a generic error must not park a model")
	}
}

func TestOAuthRefreshExchange(t *testing.T) {
	jwk, err := NewDPoPKey()
	if err != nil {
		t.Fatal(err)
	}
	var sawDPoP string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != TokenPath {
			t.Errorf("path = %s, want %s", r.URL.Path, TokenPath)
		}
		sawDPoP = r.Header.Get("DPoP")
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			t.Errorf("content type = %s", r.Header.Get("Content-Type"))
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.PostForm.Get("grant_type"); got != "refresh_token" {
			t.Errorf("grant_type = %q", got)
		}
		if got := r.PostForm.Get("client_id"); got != ClientID {
			t.Errorf("client_id = %q", got)
		}
		if got := r.PostForm.Get("refresh_token"); got != "refresh-1" {
			t.Errorf("refresh_token = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"user_id":"u1","refresh_token":"refresh-2","credentials":{
			"access_key_id":"AK2","secret_access_key":"SK2","security_token":"STS2",
			"expiration":"2026-11-18T00:00:00Z"}}`)
	}))
	defer srv.Close()

	res, err := Refresh(t.Context(), srv.Client(), srv.URL, "refresh-1", "verifier", jwk)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if res.Credentials.SecurityToken != "STS2" || res.Credentials.AccessKeyID != "AK2" || res.RefreshToken != "refresh-2" {
		t.Fatalf("unexpected token response: %+v", res)
	}
	if got := res.Expiry().Unix(); got != time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("expiry = %d", got)
	}

	// The DPoP proof must target the exact token URL and use POST.
	parts := strings.Split(sawDPoP, ".")
	if len(parts) != 3 {
		t.Fatalf("DPoP is not a JWT: %q", sawDPoP)
	}
	payload := decodeSegment(t, parts[1])
	if !strings.Contains(payload, `"htm":"POST"`) {
		t.Fatalf("DPoP htm: %s", payload)
	}
	if !strings.Contains(payload, `"htu":"`+srv.URL+TokenPath+`"`) {
		t.Fatalf("DPoP htu: %s", payload)
	}
	header := decodeSegment(t, parts[0])
	if !strings.Contains(header, `"alg":"ES256"`) || !strings.Contains(header, `"jwk"`) {
		t.Fatalf("DPoP header: %s", header)
	}
}

func TestOAuthRefreshTerminalFailure(t *testing.T) {
	jwk, _ := NewDPoPKey()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant","error_code":"ExpiredRefreshToken","error_msg":"expired"}`)
	}))
	defer srv.Close()
	_, err := Refresh(t.Context(), srv.Client(), srv.URL, "refresh-1", "", jwk)
	if !errors.Is(err, ErrRefreshExpired) {
		t.Fatalf("err = %v, want ErrRefreshExpired", err)
	}
}

func TestOAuthRefreshWithoutDPoPKeyIsTerminal(t *testing.T) {
	_, err := Refresh(t.Context(), nil, "https://example.invalid", "refresh-1", "", nil)
	if !errors.Is(err, ErrRefreshExpired) {
		t.Fatalf("err = %v, want ErrRefreshExpired", err)
	}
}

func TestRefreshWithoutTokenIsTerminal(t *testing.T) {
	_, err := Refresh(t.Context(), nil, "https://example.invalid", "", "", nil)
	if !errors.Is(err, ErrRefreshExpired) {
		t.Fatalf("err = %v, want ErrRefreshExpired", err)
	}
}
