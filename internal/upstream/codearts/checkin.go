package codearts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
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
	ID                 json.RawMessage `json:"campaignId"`
	Type               string          `json:"type"`
	Title              string          `json:"title"`
	Status             string          `json:"status"`
	Claimable          bool            `json:"claimable"`
	BenefitUnit        string          `json:"benefitUnit"`
	BenefitAmount      float64         `json:"benefitAmount"`
	PendingTotalAmount float64         `json:"pendingTotalAmount"`
	Extra              struct {
		StartTime string `json:"startTime"`
		EndTime   string `json:"endTime"`
	} `json:"extra"`
}

// Key normalizes numeric and string IDs for local bookkeeping. The signed
// write still carries the value with the JSON type returned by the vendor.
func (c Campaign) Key() string {
	var id string
	if json.Unmarshal(c.ID, &id) == nil {
		return id
	}
	var n json.Number
	if json.Unmarshal(c.ID, &n) == nil {
		return n.String()
	}
	return ""
}

func (c Campaign) IsDailyCredit() bool {
	return strings.EqualFold(strings.TrimSpace(c.Type), "USER_LOGIN") &&
		strings.EqualFold(strings.TrimSpace(c.BenefitUnit), "CREDIT")
}

func (c Campaign) State() string { return strings.ToUpper(strings.TrimSpace(c.Status)) }

func (c Campaign) Confirmed() bool {
	return !c.Claimable && (c.State() == "CONFIRMED" || c.State() == "CONSUMED")
}

func (c Campaign) WithinPeriod(now time.Time) bool {
	if start, err := time.Parse(time.RFC3339Nano, c.Extra.StartTime); err == nil && now.Before(start) {
		return false
	}
	if end, err := time.Parse(time.RFC3339Nano, c.Extra.EndTime); err == nil && !now.Before(end) {
		return false
	}
	return true
}

type CreditHTTPError struct {
	Path, Body string
	Status     int
	RetryAfter string
}

func (e *CreditHTTPError) Error() string {
	return fmt.Sprintf("codearts %s HTTP %d: %s", e.Path, e.Status, e.Body)
}

type CreditBusinessError struct {
	Path, Message string
	Code          int
}

func (e *CreditBusinessError) Error() string {
	return fmt.Sprintf("codearts %s: code=%d message=%s", e.Path, e.Code, e.Message)
}

func CreditRetryTime(err error, now time.Time) time.Time {
	var httpErr *CreditHTTPError
	if !errors.As(err, &httpErr) || (httpErr.Status != http.StatusTooManyRequests && httpErr.Status != http.StatusServiceUnavailable) {
		return time.Time{}
	}
	if seconds, convErr := strconv.ParseInt(httpErr.RetryAfter, 10, 32); convErr == nil && seconds > 0 {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	t, _ := http.ParseTime(httpErr.RetryAfter)
	return t
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
		return nil, &CreditHTTPError{Path: path, Status: resp.StatusCode, Body: truncate(string(response), 300), RetryAfter: resp.Header.Get("Retry-After")}
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
		return nil, &CreditBusinessError{Path: path, Code: *envelope.Code, Message: truncate(envelope.Message, 200)}
	}
	return envelope.Data, nil
}

// ClaimCredit and ConfirmCredit are separate protocol steps. The account
// service assigns and persists the claim's stable key before either write.
func ClaimCredit(ctx context.Context, hc *http.Client, base string, cred Credential, c Campaign, key string) error {
	raw, err := creditRequest(ctx, hc, base, http.MethodPost, ClaimPath, map[string]any{
		"campaignId": c.ID, "channel": CreditChannel, "idempotentKey": key,
	}, cred)
	if err != nil {
		return err
	}
	var receipt Campaign
	if err := json.Unmarshal(raw, &receipt); err != nil || receipt.Key() == "" || receipt.Key() != c.Key() {
		return errors.New("codearts: claim receipt campaign ID does not match")
	}
	return nil
}

func ConfirmCredit(ctx context.Context, hc *http.Client, base string, cred Credential, c Campaign) error {
	_, err := creditRequest(ctx, hc, base, http.MethodPost, ConfirmPath, map[string]any{"campaignId": c.ID}, cred)
	return err
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
