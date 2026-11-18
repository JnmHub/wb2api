package codearts

import (
	"math"
	"net/http"
	"strings"
	"testing"

	"wb2api/internal/upstream/identity"
)

// trialDocument is the shape a real trial account returned: the summary row
// equals the itemised rows, so counting both would double the balance.
const trialDocument = `{
  "end_date": "2026-12-01",
  "package": {"spec_code":"codearts.agent.trial","package_name_cn":"试用版","package_name_en":"Trial","package_url":"https://example.invalid/p"},
  "metrics": [
    {"name":"usageTotalPackageCredit","show":true,"package_credit_amount":10345.14,"package_credit_used":5172.57,"package_credit_remain":5172.57,"value":50.0},
    {"name":"usageBasicPackageCredit","show":true,"package_credit_amount":345.14,"package_credit_used":172.57,"package_credit_remain":172.57,"value":50.0},
    {"name":"usageOnDemandPackageCredit","show":true,"package_credit_amount":0,"package_credit_used":0,"package_credit_remain":0,"value":0.0},
    {"name":"usageBonusPackageCredit","show":true,"package_credit_amount":10000,"package_credit_used":5000,"package_credit_remain":5000,"value":50.0},
    {"name":"usageTokenChatMessages","show":true,"usage_token_num":123456,"package_token_amount":500000,"value":24.7},
    {"name":"usageDataChatMessages","show":false,"value":-1},
    {"name":"retiredMetric","show":true,"usage_token_num":0,"package_token_amount":0,"value":-1}
  ]
}`

func TestParseStatisticsAndAggregate(t *testing.T) {
	st, err := ParseStatistics([]byte(trialDocument))
	if err != nil {
		t.Fatal(err)
	}
	if st.Plan != "codearts.agent.trial" || st.PlanName != "Trial" || st.ResetDate != "2026-12-01" {
		t.Fatalf("plan = %+v", st)
	}
	// show:false is skipped, the empty retired row (only a -1 sentinel) too.
	if len(st.Meters) != 5 {
		t.Fatalf("meters = %d: %+v", len(st.Meters), st.Meters)
	}
	for _, m := range st.Meters {
		if m.Name == "usageDataChatMessages" || m.Name == "retiredMetric" {
			t.Fatalf("hidden/empty row kept: %+v", m)
		}
		if m.UsedPercent != nil && *m.UsedPercent < 0 {
			t.Fatalf("negative sentinel kept as a percentage: %+v", m)
		}
	}
	if st.Meters[0].Label != "套餐积分" {
		t.Fatalf("label = %q", st.Meters[0].Label)
	}

	b := st.Aggregate()
	// The summary row duplicates the itemised rows: 172.57 + 0 + 5000.
	if math.Abs(b.Remaining-5172.57) > 1e-6 {
		t.Fatalf("remaining = %v, want 5172.57", b.Remaining)
	}
	if math.Abs(b.Total-10345.14) > 1e-6 {
		t.Fatalf("total = %v, want 10345.14", b.Total)
	}
	if math.Abs(b.Used-5172.57) > 1e-6 {
		t.Fatalf("used = %v", b.Used)
	}
}

// When the itemised rows do not add up to the summary, the upstream's own
// numbers disagree and both are kept rather than dropping real money.
func TestAggregateKeepsSummaryWhenItDoesNotMatch(t *testing.T) {
	doc := `{"metrics":[
		{"name":"usageTotalPackageCredit","show":true,"package_credit_remain":1500},
		{"name":"usageBasicPackageCredit","show":true,"package_credit_remain":700},
		{"name":"usageOnDemandPackageCredit","show":true,"package_credit_remain":100}
	]}`
	st, err := ParseStatistics([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	b := st.Aggregate()
	if math.Abs(b.Remaining-2300) > 1e-6 {
		t.Fatalf("remaining = %v, want 1500+700+100", b.Remaining)
	}
}

func TestMeterBalanceView(t *testing.T) {
	rem, total := 172.57, 345.14
	m := Meter{CreditRemain: &rem, CreditTotal: &total}
	if got := m.BalanceView(); !strings.Contains(got, "剩 172.57 / 345.14 积分") {
		t.Fatalf("credit view = %q", got)
	}
	tok := Meter{UsedTokens: 123456, AllowanceTokens: 500000}
	pct := 24.7
	tok.UsedPercent = &pct
	if got := tok.BalanceView(); got != "已用 123456 / 500000 token（24.7%）" {
		t.Fatalf("token view = %q", got)
	}
	// A one-token "package" is the upstream's placeholder shape, not a quota.
	placeholder := Meter{UsedTokens: 0, AllowanceTokens: 1}
	if got := placeholder.BalanceView(); got != "" {
		t.Fatalf("placeholder view = %q, want empty", got)
	}
	pctOnly := Meter{UsedPercent: &pct}
	if got := pctOnly.BalanceView(); got != "已用 24.7%" {
		t.Fatalf("percent-only view = %q", got)
	}
}

func TestStatisticsSignatureCoversFourHeaders(t *testing.T) {
	// The statistics call signs exactly Accept / X-Language / plugin-name /
	// plugin-version: adding Content-Type would change the signed string.
	h := identity.Headers{
		identity.P("Accept", "application/json"),
		identity.P("X-Language", "zh-cn"),
		identity.P("plugin-name", PluginName),
		identity.P("plugin-version", PluginVersion),
	}
	signed := Signer{}.SignHeaders(h, http.MethodGet, "https://snap-access.cn-north-4.myhuaweicloud.com"+StatisticsPath, nil, Credential{AccessKeyID: "AK", SecretAccessKey: "SK"})
	auth, _ := signed.Get("Authorization")
	if !strings.Contains(auth, "SignedHeaders=accept;host;plugin-name;plugin-version;x-language;x-sdk-content-sha256;x-sdk-date") {
		t.Fatalf("signed headers = %s", auth)
	}
	if strings.Contains(auth, "content-type") {
		t.Fatalf("Content-Type leaked into the signature: %s", auth)
	}
}
