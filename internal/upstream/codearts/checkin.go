package codearts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"wb2api/internal/upstream/identity"
)

// Credit endpoints live on the snap root (not under /snap-manager).
const (
	DeliveryPath  = "/v1/ops/delivery"
	ClaimPath     = "/v1/ops/claim"
	ConfirmPath   = "/v1/ops/confirm"
	CreditChannel = "IDE"
)

// Campaign is one entry of the credit delivery list. The activity ticket in the
// upstream payload is deliberately not kept: only the identity and the state
// machine matter here.
type Campaign struct {
	ID                 int     `json:"campaignId"`
	Title              string  `json:"title"`
	Status             string  `json:"status"`
	Claimable          bool    `json:"claimable"`
	BenefitUnit        string  `json:"benefitUnit"`
	BenefitAmount      float64 `json:"benefitAmount"`
	PendingTotalAmount float64 `json:"pendingTotalAmount"`
	Extra              struct {
		StartTime string `json:"startTime"`
		EndTime   string `json:"endTime"`
	} `json:"extra"`
}

// ClaimResult is the outcome of one campaign.
type ClaimResult struct {
	CampaignID int     `json:"campaign_id"`
	Title      string  `json:"title"`
	OK         bool    `json:"ok"`
	Status     string  `json:"status"`
	Credits    float64 `json:"credits,omitempty"`
	Message    string  `json:"message"`
}

// CheckinResult reports one account's check-in. Credits is what this call
// claimed, not a balance.
type CheckinResult struct {
	OK        bool          `json:"ok"`
	Credits   float64       `json:"credits,omitempty"`
	Message   string        `json:"message"`
	Campaigns []ClaimResult `json:"campaigns"`
}

// creditRequest signs and sends one credit operation and unwraps the
// {code,message,data} envelope.
func creditRequest(ctx context.Context, hc *http.Client, base, method, path string, body any, cred Credential) (json.RawMessage, error) {
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	target := strings.TrimRight(base, "/") + path
	h := identity.Headers{
		identity.P("Content-Type", "application/json"),
		identity.P("Agent-Type", "PromptCenter"),
		identity.P("X-Language", "zh-cn"),
	}
	h = Signer{}.SignHeaders(h, method, target, raw, cred)

	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	for _, p := range h {
		if strings.EqualFold(p.Name, "Host") {
			continue // net/http writes Host from the URL
		}
		req.Header.Set(p.Name, p.Value)
	}
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	response, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("codearts %s HTTP %d: %s", path, resp.StatusCode, truncate(string(response), 300))
	}
	var envelope struct {
		Code    *int            `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(response, &envelope); err != nil {
		return nil, fmt.Errorf("codearts %s: parse response: %w", path, err)
	}
	if envelope.Code == nil {
		return nil, fmt.Errorf("codearts %s: response has no business code", path)
	}
	if *envelope.Code != 0 {
		return nil, fmt.Errorf("codearts %s: code=%d message=%s", path, *envelope.Code, truncate(envelope.Message, 200))
	}
	return envelope.Data, nil
}

// FetchCampaigns lists the credit activities of one account.
func FetchCampaigns(ctx context.Context, hc *http.Client, base string, cred Credential) ([]Campaign, error) {
	raw, err := creditRequest(ctx, hc, base, http.MethodGet, DeliveryPath+"?channel="+CreditChannel, nil, cred)
	if err != nil {
		return nil, err
	}
	var data struct {
		Items *[]Campaign `json:"items"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("codearts: parse campaigns: %w", err)
	}
	if data.Items == nil {
		return nil, fmt.Errorf("codearts: campaign list has no items")
	}
	return *data.Items, nil
}

// Checkin claims the daily sign-in (campaign 1) and the new-user reward
// (campaign 4). Eligibility and the daily refresh are decided by the upstream:
// a CLAIMED campaign is only confirmed, never claimed twice, which is why the
// caller must serialise calls per account.
func Checkin(ctx context.Context, hc *http.Client, base string, cred Credential, now time.Time) (CheckinResult, error) {
	campaigns, err := FetchCampaigns(ctx, hc, base, cred)
	if err != nil {
		return CheckinResult{Message: err.Error()}, err
	}
	out := CheckinResult{OK: true, Campaigns: make([]ClaimResult, 0, len(campaigns))}
	for _, c := range campaigns {
		if c.ID != 1 && c.ID != 4 {
			continue
		}
		if c.BenefitUnit != "" && c.BenefitUnit != "CREDIT" {
			continue
		}
		res := claimCampaign(ctx, hc, base, cred, c, now)
		out.Campaigns = append(out.Campaigns, res)
		out.OK = out.OK && res.OK
		out.Credits += res.Credits
	}
	parts := make([]string, 0, len(out.Campaigns))
	for _, res := range out.Campaigns {
		parts = append(parts, res.Title+": "+res.Message)
	}
	out.Message = strings.Join(parts, "; ")
	if len(parts) == 0 {
		out.Message = "无可领取的签到或新手活动"
	}
	return out, nil
}

func claimCampaign(ctx context.Context, hc *http.Client, base string, cred Credential, c Campaign, now time.Time) ClaimResult {
	res := ClaimResult{CampaignID: c.ID, Title: c.Title, Status: strings.TrimSpace(c.Status), OK: true}
	if res.Title == "" {
		res.Title = fmt.Sprintf("积分活动 %d", c.ID)
	}
	if end, err := time.Parse(time.RFC3339Nano, c.Extra.EndTime); err == nil && !now.Before(end) {
		res.Message = "活动已结束"
		return res
	}
	if start, err := time.Parse(time.RFC3339Nano, c.Extra.StartTime); err == nil && now.Before(start) {
		res.Message = "活动尚未开始"
		return res
	}
	switch res.Status {
	case "CONFIRMED", "CONSUMED":
		res.Message = "已领取"
		return res
	case "CLAIMED":
		// Credited but not confirmed: never claim again, only confirm.
	case "ELIGIBLE":
		if !c.Claimable {
			res.OK, res.Message = false, "暂不可领取，稍后重试"
			return res
		}
		_, err := creditRequest(ctx, hc, base, http.MethodPost, ClaimPath, map[string]any{
			"campaignId":    c.ID,
			"channel":       CreditChannel,
			"idempotentKey": fmt.Sprintf("claim_%d_%d", c.ID, now.UnixMilli()),
		}, cred)
		if err != nil {
			res.OK, res.Message = false, err.Error()
			return res
		}
		res.Status = "CLAIMED"
		res.Credits = c.PendingTotalAmount
		if res.Credits <= 0 {
			res.Credits = c.BenefitAmount
		}
	case "":
		res.Message = "未获得活动资格"
		return res
	default:
		res.OK, res.Message = false, "未知活动状态: "+res.Status
		return res
	}
	if _, err := creditRequest(ctx, hc, base, http.MethodPost, ConfirmPath, map[string]any{"campaignId": c.ID}, cred); err != nil {
		res.OK, res.Message = false, "已领取，确认失败，稍后重试: "+err.Error()
		return res
	}
	res.Status = "CONFIRMED"
	res.Message = "已补确认"
	if res.Credits > 0 {
		res.Message = fmt.Sprintf("领取成功（%g 积分）", res.Credits)
	}
	return res
}
