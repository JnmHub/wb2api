package raccoon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"wb2api/internal/jsonx"
	"wb2api/internal/store/accounts"
)

// token builds an unsigned JWT with the given claims (the gateway never
// verifies the signature; it only reads payload fields).
func token(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestUserIDExtraction(t *testing.T) {
	// A legacy token: sid is not hex, iss is ("6d446a" -> 7160938).
	legacy := token(t, map[string]any{"sid": "web65aa393e65a6b6d446a51dde22c-a6d6-48e2", "iss": "6d446a", "exp": 4102444800})
	if got := UserID(legacy); got != "7160938" {
		t.Fatalf("legacy UserID = %q, want 7160938", got)
	}
	// sid wins when it is hex.
	bySID := token(t, map[string]any{"sid": "0a", "iss": "ff"})
	if got := UserID(bySID); got != "10" {
		t.Fatalf("sid UserID = %q, want 10", got)
	}
	// IAM tokens use sub and must not go through the hex conversion.
	iam := token(t, map[string]any{"iss": "xiaohuanxiong.com/iam", "sub": "u-42", "exp": 4102444800})
	if got := UserID(iam); got != "u-42" {
		t.Fatalf("iam UserID = %q, want u-42", got)
	}
	if !IsIAM(iam) || IsIAM(legacy) {
		t.Fatal("IAM detection is wrong")
	}
	if Expired(iam, time.Unix(1700000000, 0)) {
		t.Fatal("token with a future exp must not be expired")
	}
}

func TestThinkingNormalize(t *testing.T) {
	body := func(raw string) *jsonx.Obj {
		obj, err := jsonx.UnmarshalObj([]byte(raw))
		if err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		return obj
	}
	// thinking family: "off" becomes the boolean switch, named levels survive.
	out := NormalizeBody(body(`{"reasoning_effort":"off"}`), "sn-deepseek-v4-1-flash-1017")
	if _, ok := out.Get("reasoning_effort").(string); ok {
		t.Fatal("off must be rewritten, not forwarded")
	}
	if th, _ := out.Obj("thinking"); th == nil {
		t.Fatal("thinking switch missing")
	} else if typ, _ := th.Str("type"); typ != "disabled" {
		t.Fatalf("thinking.type = %q, want disabled", typ)
	}
	out = NormalizeBody(body(`{"reasoning_effort":"high"}`), "sn-deepseek-v4-1-flash-1017")
	if effort, _ := out.Str("reasoning_effort"); effort != "high" {
		t.Fatalf("named level must survive, got %q", effort)
	}

	// low_high family squashes to low/high.
	for _, c := range []struct{ in, want string }{{"minimal", "low"}, {"low", "low"}, {"medium", "high"}, {"high", "high"}, {"off", "low"}} {
		out := NormalizeBody(body(`{"reasoning_effort":"`+c.in+`"}`), "sn-glm-5-3")
		if got, _ := out.Str("reasoning_effort"); got != c.want {
			t.Fatalf("sn-glm-5-3 effort %q -> %q, want %q", c.in, got, c.want)
		}
	}
	// The caller's payload must stay untouched.
	src := body(`{"reasoning_effort":"off"}`)
	NormalizeBody(src, "sn-glm-5-3")
	if got, _ := src.Str("reasoning_effort"); got != "off" {
		t.Fatalf("NormalizeBody mutated its input: %q", got)
	}
	if !SupportsThinking("raccoon-8c4485") || SupportsThinking("gpt-5.6-sol") {
		t.Fatal("family detection is wrong")
	}
}

func TestClassify(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cases := []struct {
		name   string
		status int
		body   string
		want   Class
	}{
		{"http 401", 401, `{}`, ClassTokenExpired},
		{"business 200003", 200, `{"code":200003,"message":"token"}`, ClassTokenExpired},
		{"authorization_verify_error", 200, `{"message":"authorization_verify_error"}`, ClassTokenExpired},
		{"quota code", 200, `{"code":1000007,"message":"积分不足"}`, ClassQuota},
		{"quota text", 400, `{"message":"insufficient_points"}`, ClassQuota},
		{"rate limited", 429, `{"code":"rate_limit"}`, ClassRateLimited},
		{"blocked", 403, `{"message":"blocked by security policy"}`, ClassBlocked},
		{"fatal", 500, `{"message":"boom"}`, ClassFatal},
	}
	for _, c := range cases {
		if got := Classify(c.status, []byte(c.body), now).Class; got != c.want {
			t.Fatalf("%s: class = %v, want %v", c.name, got, c.want)
		}
	}
	if reset := Classify(429, []byte(`{"code":"rate_limit"}`), now).Reset; !reset.After(now) {
		t.Fatal("a transient 429 must carry a reset instant")
	}
	if reset := Classify(200, []byte(`{"code":1000007}`), now).Reset; !reset.IsZero() {
		t.Fatal("a quota failure is not a transient rate limit")
	}
}

func TestParseCallback(t *testing.T) {
	state := "abc123"
	good := "office-raccoon://auth/callback?code=CODE&state=" + state
	if code, err := ParseCallback(good, state); err != nil || code != "CODE" {
		t.Fatalf("good callback: code=%q err=%v", code, err)
	}
	if _, err := ParseCallback("office-raccoon://auth/callback?code=CODE&state=other", state); err == nil {
		t.Fatal("a wrong state must be rejected")
	}
	for _, bad := range []string{
		"office-raccoon://auth/callback@evil.com?code=CODE&state=" + state,
		"office-raccoon://auth/other?code=CODE&state=" + state,
		"https://auth/callback?code=CODE&state=" + state,
	} {
		if _, err := ParseCallback(bad, state); err == nil {
			t.Fatalf("%s must be rejected", bad)
		}
	}
	if !IsLoopbackHTTP("http", "localhost", "8788") ||
		IsLoopbackHTTP("https", "localhost", "8788") ||
		IsLoopbackHTTP("http", "10.0.0.5", "8788") ||
		IsLoopbackHTTP("http", "localhost", "") {
		t.Fatal("loopback detection is wrong")
	}
}

func TestCatalogParse(t *testing.T) {
	raw := `{"categories":[
		{"type":"chat","models":[
			{"name":"sn-glm-5-3","display_name":"GLM-5-3","context_window":1000000,"max_tokens":100000,
			 "supports_reasoning":true,"billing_multiplier":0.75,"visible":true},
			{"name":"sn-glm-5-3","display_name":"duplicate"},
			{"model_name":"raccoon-image","display_name":"Raccoon Image"},
			{"name":""}
		]},
		{"type":"embed","models":[{"name":"emb"}]}
	]}`
	mm := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &mm); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	models := ParseCatalog(mm)
	if len(models) != 2 {
		t.Fatalf("models = %d, want 2 (%+v)", len(models), models)
	}
	if models[0].ID != "sn-glm-5-3" || models[0].Name != "GLM-5-3" ||
		models[0].ContextWindow != 1000000 || models[0].MaxTokens != 100000 ||
		!models[0].SupportsThinking || models[0].Multiplier != 0.75 {
		t.Fatalf("first model parsed wrong: %+v", models[0])
	}
	if models[1].Kind != "image" {
		t.Fatalf("image entry not recognised: %+v", models[1])
	}
	ids := ModelsOf(models)
	if len(ids) != 1 || ids[0] != "sn-glm-5-3" {
		t.Fatalf("ModelsOf = %v, want [sn-glm-5-3] (image filtered)", ids)
	}
	if !SupportsModel(models, "SN-GLM-5-3") || SupportsModel(models, "gpt-4o") {
		t.Fatal("SupportsModel is wrong")
	}
	if len(ModelsOf(nil)) != 0 || !SupportsModel(nil, "anything") {
		t.Fatal("an undiscovered catalogue must allow everything")
	}
}

// raccoonServer fakes the three origins the channel talks to.
func raccoonServer(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Client{
		HTTP:     srv.Client(),
		LlmBase:  srv.URL + "/api/web/llm/v2",
		AuthBase: srv.URL + "/api/web/auth/v1",
		Origin:   srv.URL,
	}
}

func TestClaimDailyNeedsLedgerConfirmation(t *testing.T) {
	billsWithGrant := false
	client := raccoonServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/web/office/v3/setting_info":
			_, _ = w.Write([]byte(`{"code":0,"data":{"point_grant_popups":["+100"],"point_grant_toast":"ok"}}`))
		case "/api/web/points/v2/bills":
			if billsWithGrant {
				_, _ = w.Write([]byte(`{"code":0,"data":{"items":[
					{"biz_type":"daily_grant","event_name":"每日积分发放","points":100,
					 "created_at":"2026-11-20T00:10:00+08:00","expire_at":"2026-12-20"}]}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"items":[]}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	})
	cred := Credential{Token: "t"}
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)

	// The trigger ran, but the ledger shows nothing: never report success.
	res, err := client.ClaimDaily(context.Background(), cred, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if res.Success {
		t.Fatalf("success without a ledger entry: %+v", res)
	}
	if res.Message != "今日未见积分入账，尚未确认领取" {
		t.Fatalf("message = %q", res.Message)
	}

	billsWithGrant = true
	res, err = client.ClaimDaily(context.Background(), cred, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !res.Success || res.Credit() != "100 积分" {
		t.Fatalf("granted result wrong: %+v credit=%q", res, res.Credit())
	}
}

func TestClaimSignupSkipsSettledKeys(t *testing.T) {
	var paths []string
	client := raccoonServer(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = w.Write([]byte(`{"code":0,"data":{"granted":true,"popup":{"points":3000}}}`))
	})
	settled := map[string]int64{"desktop_login_grant": 1}
	out, err := client.ClaimSignup(context.Background(), Credential{Token: "t"}, settled)
	if err != nil {
		t.Fatalf("claim signup: %v", err)
	}
	if len(out) != 1 || out[0].Key != "mobile_login_grant" || out[0].Points != 3000 || !out[0].Granted {
		t.Fatalf("outcomes = %+v", out)
	}
	if len(paths) != 1 || paths[0] != MobileGrantPath {
		t.Fatalf("settled task was requested: %v", paths)
	}
	merged := SettleOutcomes(settled, out, time.Unix(1700000000, 0))
	if merged["desktop_login_grant"] != 1 || merged["mobile_login_grant"] == 0 {
		t.Fatalf("ledger merge wrong: %v", merged)
	}
	// A transport failure must not settle the task.
	failed := SettleOutcomes(nil, []ClaimOutcome{{Key: "mobile_login_grant", Err: "boom"}}, time.Now())
	if _, ok := failed["mobile_login_grant"]; ok {
		t.Fatal("a failed probe must stay unsettled")
	}
}

func TestCredentialRoundTrip(t *testing.T) {
	exp := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	rec := &accounts.Record{
		UID: "rc-7160938", Realm: "raccoon",
		AccessToken: "old", RefreshToken: "rt", ExpiresAt: exp.Unix(),
		Extra: map[string]any{ExtraKey: map[string]any{
			"userId": "7160938", "officeIdentity": "org-9", "models": []any{"sn-glm-5-3"},
		}},
	}
	cred := CredentialOf(rec)
	if cred.UserID != "7160938" || cred.OrgCode != "org-9" || cred.IAM || cred.ExpiresAt.Unix() != exp.Unix() {
		t.Fatalf("credential = %+v", cred)
	}
	next := ApplyToken(rec, Credential{Token: "new", RefreshToken: "rt2", ExpiresAt: exp.Add(time.Hour)})
	if next.AccessToken != "new" || next.RefreshToken != "rt2" {
		t.Fatalf("ApplyToken = %+v", next)
	}
	if rec.AccessToken != "old" {
		t.Fatal("ApplyToken mutated the source record")
	}
	if got := ModelsOfRecord(next); len(got) != 1 || got[0] != "sn-glm-5-3" {
		t.Fatalf("models survived ApplyToken: %v", got)
	}
	// An IAM credential has no refresh token and cannot be refreshed.
	iamToken := token(t, map[string]any{"iss": "xiaohuanxiong.com/iam", "sub": "u1", "exp": 4102444800})
	iam := ApplyToken(rec, Credential{Token: iamToken, RefreshToken: "stale", IAM: true})
	if iam.RefreshToken != "" {
		t.Fatalf("IAM credential kept a refresh token: %q", iam.RefreshToken)
	}
	if !NeedsLogin(&accounts.Record{UID: "x", Realm: "raccoon"}) {
		t.Fatal("an account without a token needs a login")
	}
}

func TestAllowsModelRejectsUnknownNames(t *testing.T) {
	rec := &accounts.Record{UID: "rc-1", Realm: "raccoon",
		Extra: map[string]any{ExtraKey: map[string]any{"models": []any{"sn-glm-5-3", "raccoon-090dd3"}}}}
	if !AllowsModel(rec, "sn-glm-5-3") || !AllowsModel(rec, "SN-GLM-5-3") {
		t.Fatal("a catalogued model must be allowed (case-insensitive)")
	}
	if !AllowsModel(rec, "raccoon/sn-glm-5-3") {
		t.Fatal("the path spelling must resolve to the wire name")
	}
	if AllowsModel(rec, "gpt-5.5") {
		t.Fatal("an unknown model must be refused: the upstream would answer with its default")
	}
	bare := &accounts.Record{UID: "rc-2", Realm: "raccoon"}
	if !AllowsModel(bare, "deepseek-v4.1-flash") || AllowsModel(bare, "") ||
		AllowsModel(bare, "gpt-5.5") || AllowsModel(bare, ImageModelID) {
		t.Fatal("an undiscovered account must allow only shipped chat models")
	}
}

func TestModelAliases(t *testing.T) {
	// A vendor-neutral name resolves to the channel's id, so one client config
	// can name the same family on every channel.
	if got := WireName("deepseek-v4.1-flash"); got != "sn-deepseek-v4-1-flash-1017" {
		t.Fatalf("alias = %q", got)
	}
	if got := WireName("DeepSeek-V4.1-Flash"); got != "sn-deepseek-v4-1-flash-1017" {
		t.Fatalf("case-insensitive alias = %q", got)
	}
	if got := WireName("raccoon/deepseek-v4.1-flash"); got != "sn-deepseek-v4-1-flash-1017" {
		t.Fatalf("path + alias = %q", got)
	}
	// Catalogue ids and unknown names pass through untouched.
	if got := WireName("sn-glm-5-3"); got != "sn-glm-5-3" {
		t.Fatalf("catalogue id changed: %q", got)
	}
	if got := WireName("gpt-5.5"); got != "gpt-5.5" {
		t.Fatalf("foreign name changed: %q", got)
	}
	// An alias counts as allowed once the catalogue has the target id.
	rec := &accounts.Record{UID: "rc-1", Realm: "raccoon",
		Extra: map[string]any{ExtraKey: map[string]any{"models": []any{"sn-deepseek-v4-1-flash-1017"}}}}
	if !AllowsModel(rec, "deepseek-v4.1-flash") {
		t.Fatal("aliased model must be allowed when the target is catalogued")
	}
	if AllowsModel(rec, "deepseek-v4-pro") {
		t.Fatal("an unmapped foreign name must stay refused")
	}
	if !KnownModel("deepseek-v4.1-flash") || KnownModel("gpt-5.5") {
		t.Fatal("KnownModel must understand aliases")
	}
}

func TestPrepareBodyWritesResolvedModel(t *testing.T) {
	raw, err := jsonx.UnmarshalObj([]byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	body, err := PrepareBody(raw, WireName("deepseek-v4.1-flash"))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	obj, err := jsonx.UnmarshalObj(body)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	// The alias must reach the upstream as its real id, otherwise the upstream
	// silently answers with its own default model.
	if got, _ := obj.Str("model"); got != "sn-deepseek-v4-1-flash-1017" {
		t.Fatalf("wire model = %q", got)
	}
	if streaming, _ := obj.Get("stream").(bool); !streaming {
		t.Fatal("stream must be forced")
	}
}

func TestPrepareBodyWithHistoryAccounting(t *testing.T) {
	const model = "sn-deepseek-v4-1-flash-1017"
	const begin = `{"messages":[`
	const old = `{"role":"user","content":"旧问题"},{"role":"assistant","content":"Old answer."},`
	const current = `{"role":"user","content":"当前轮"}]}`
	payload, err := jsonx.UnmarshalObj([]byte(begin + old + current))
	if err != nil {
		t.Fatal(err)
	}
	full, err := PrepareBody(payload, model)
	if err != nil {
		t.Fatal(err)
	}
	activePayload, err := jsonx.UnmarshalObj([]byte(begin + current))
	if err != nil {
		t.Fatal(err)
	}
	active, err := PrepareBody(activePayload, model)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		limit, wantRemoved int
		want               []byte
	}{
		{limit: 0, want: full},
		{limit: len(full), want: full},
		{limit: len(active), want: active, wantRemoved: 12},
		{limit: 1, want: active, wantRemoved: 12},
	} {
		got, removed, err := PrepareBodyWithHistoryAccounting(payload, model, test.limit)
		if err != nil || string(got) != string(test.want) || removed != test.wantRemoved {
			t.Fatalf("limit = %d, body = %s, removed tokens = %d, error = %v; want %s and %d removed tokens",
				test.limit, got, removed, err, test.want, test.wantRemoved)
		}
	}
}
