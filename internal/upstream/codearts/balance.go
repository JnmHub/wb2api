package codearts

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"

	"wb2api/internal/upstream/identity"
)

// StatisticsPath is the subscription statistics endpoint: the only place the
// upstream exposes a credit balance. It lives on snap-access under
// /snap-manager (unlike the STS token endpoint).
const StatisticsPath = "/snap-manager/v1/statistics/plugin"

// Plugin identity used by the statistics signature. The signature covers
// exactly the four headers the official client sends for this call — adding
// Content-Type or Agent-Type would change the signed string.
const (
	statisticsPluginName    = PluginName
	statisticsPluginVersion = PluginVersion
)

// Meter is one row of the subscription statistic table. The upstream fills
// either the token pair (used/allowance) or the credit trio per metric type.
type Meter struct {
	Name            string   `json:"name"`
	Label           string   `json:"label"`
	UsedTokens      int64    `json:"used_tokens"`
	AllowanceTokens int64    `json:"allowance_tokens"`
	CreditTotal     *float64 `json:"credit_total,omitempty"`
	CreditUsed      *float64 `json:"credit_used,omitempty"`
	CreditRemain    *float64 `json:"credit_remain,omitempty"`
	// UsedPercent is only kept when the upstream value is >= 0: -1 is its
	// "not applicable" sentinel, not a percentage.
	UsedPercent *float64 `json:"used_percent,omitempty"`
}

// Statistics is the parsed subscription document.
type Statistics struct {
	ResetDate string  `json:"reset_date"`
	Plan      string  `json:"plan"`
	PlanName  string  `json:"plan_name"`
	PlanURL   string  `json:"plan_url"`
	Meters    []Meter `json:"meters"`
}

// Balance is the aggregated credit position.
//
// The "total package" row is a summary, not a separate account: its remaining
// equals the sum of the itemised rows. Counting both would double the balance,
// so a summary row is dropped when it matches the itemised sum.
type Balance struct {
	Remaining    float64
	Total        float64
	Used         float64
	HasRemaining bool
	HasTotal     bool
	ResetDate    string
	Plan         string
	PlanName     string
	Meters       []Meter
}

// meterLabels mirrors the upstream metric names. Guessing these does not work:
// the real names are usageTokenChatMessages / usageTotalPackageCredit and
// friends, so anything unknown falls back to the raw name.
var meterLabels = map[string]string{
	"usageDataCodeCompletions":   "代码补全额度",
	"usageDataChatMessages":      "对话消息额度",
	"usageTokenChatMessages":     "对话 token 额度",
	"usageTotalPackageCredit":    "套餐积分",
	"usageBasicPackageCredit":    "基础包积分",
	"usageOnDemandPackageCredit": "按需付费积分",
	"usageBonusPackageCredit":    "赠送积分",
}

// FetchStatistics reads the subscription statistics of one account.
func FetchStatistics(ctx context.Context, hc *http.Client, base string, cred Credential) (*Statistics, error) {
	url := strings.TrimRight(base, "/") + StatisticsPath
	// Four headers, exactly like the official client: the signature covers
	// every header that is sent.
	h := identity.Headers{
		identity.P("Accept", "application/json"),
		identity.P("X-Language", "zh-cn"),
		identity.P("plugin-name", statisticsPluginName),
		identity.P("plugin-version", statisticsPluginVersion),
	}
	h = Signer{}.SignHeaders(h, http.MethodGet, url, nil, cred)
	raw, err := doSigned(ctx, hc, url, http.MethodGet, h, nil)
	if err != nil {
		return nil, err
	}
	return ParseStatistics(raw)
}

// ParseStatistics turns the upstream document into meters and plan metadata.
func ParseStatistics(raw []byte) (*Statistics, error) {
	var doc struct {
		EndDate string `json:"end_date"`
		Package *struct {
			SpecCode      string `json:"spec_code"`
			PackageNameCN string `json:"package_name_cn"`
			PackageNameEN string `json:"package_name_en"`
			PackageURL    string `json:"package_url"`
		} `json:"package"`
		Metrics []struct {
			Name                string   `json:"name"`
			Show                *bool    `json:"show"`
			UsageTokenNum       int64    `json:"usage_token_num"`
			PackageTokenAmount  int64    `json:"package_token_amount"`
			PackageCreditAmount *float64 `json:"package_credit_amount"`
			PackageCreditUsed   *float64 `json:"package_credit_used"`
			PackageCreditRemain *float64 `json:"package_credit_remain"`
			Value               *float64 `json:"value"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("codearts statistics: parse response: %w", err)
	}
	out := &Statistics{ResetDate: strings.TrimSpace(doc.EndDate)}
	if p := doc.Package; p != nil {
		out.Plan = strings.TrimSpace(p.SpecCode)
		out.PlanName = strings.TrimSpace(p.PackageNameEN)
		if out.PlanName == "" {
			out.PlanName = strings.TrimSpace(p.PackageNameCN)
		}
		out.PlanURL = strings.TrimSpace(p.PackageURL)
	}
	for _, m := range doc.Metrics {
		name := strings.TrimSpace(m.Name)
		// show:false is the upstream's "do not display this row" instruction:
		// retired metrics come back with value:-1 through this flag.
		if name == "" || m.Show == nil || !*m.Show {
			continue
		}
		meter := Meter{
			Name: name, Label: meterLabels[name],
			UsedTokens: m.UsageTokenNum, AllowanceTokens: m.PackageTokenAmount,
			CreditTotal: m.PackageCreditAmount, CreditUsed: m.PackageCreditUsed,
			CreditRemain: m.PackageCreditRemain,
		}
		if meter.Label == "" {
			meter.Label = name
		}
		if m.Value != nil && *m.Value >= 0 {
			v := *m.Value
			meter.UsedPercent = &v
		}
		if meter.empty() {
			continue
		}
		out.Meters = append(out.Meters, meter)
	}
	return out, nil
}

func (m Meter) empty() bool {
	return m.UsedPercent == nil && m.UsedTokens == 0 && m.AllowanceTokens == 0 &&
		m.CreditTotal == nil && m.CreditRemain == nil
}

// BalanceView is the human line for one meter (the panel shows this verbatim).
func (m Meter) BalanceView() string {
	if m.CreditRemain != nil && m.CreditTotal != nil {
		return fmt.Sprintf("剩 %s / %s 积分", compactFloat(*m.CreditRemain), compactFloat(*m.CreditTotal))
	}
	if m.AllowanceTokens > 1 {
		pct := ""
		if m.UsedPercent != nil {
			pct = fmt.Sprintf("（%s%%）", compactFloat(*m.UsedPercent))
		}
		return fmt.Sprintf("已用 %d / %d token%s", m.UsedTokens, m.AllowanceTokens, pct)
	}
	if m.UsedTokens > 0 {
		return fmt.Sprintf("已用 %d token", m.UsedTokens)
	}
	if m.UsedPercent != nil {
		return fmt.Sprintf("已用 %s%%", compactFloat(*m.UsedPercent))
	}
	return ""
}

// Aggregate sums the credit rows into one balance.
func (s *Statistics) Aggregate() Balance {
	var out Balance
	if s == nil {
		return out
	}
	out.ResetDate, out.Plan, out.PlanName, out.Meters = s.ResetDate, s.Plan, s.PlanName, s.Meters

	partsRemain, hasParts := 0.0, false
	partsTotal, hasPartsTotal := 0.0, false
	for _, m := range s.Meters {
		if m.Name == "usageTotalPackageCredit" {
			continue
		}
		if m.CreditRemain != nil {
			partsRemain += *m.CreditRemain
			hasParts = true
		}
		if m.CreditTotal != nil {
			partsTotal += *m.CreditTotal
			hasPartsTotal = true
		}
	}
	for _, m := range s.Meters {
		if m.CreditRemain != nil {
			if !(m.Name == "usageTotalPackageCredit" && hasParts && nearlyEqual(partsRemain, *m.CreditRemain)) {
				out.Remaining += *m.CreditRemain
				out.HasRemaining = true
			}
		}
		if m.CreditTotal != nil {
			if !(m.Name == "usageTotalPackageCredit" && hasPartsTotal && nearlyEqual(partsTotal, *m.CreditTotal)) {
				out.Total += *m.CreditTotal
				out.HasTotal = true
			}
		}
	}
	if out.HasRemaining && out.HasTotal {
		out.Used = out.Total - out.Remaining
		if out.Used < 0 {
			out.Used = 0
		}
	}
	return out
}

func nearlyEqual(a, b float64) bool {
	return math.Abs(a-b) <= math.Max(math.Abs(b)*1e-6, 1e-9)
}

func compactFloat(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}
