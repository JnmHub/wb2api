package raccoon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Wallet is one credit bucket. The upstream only reports a balance per bucket
// (no denomination, no used amount, no expiry), so a missing number is skipped
// rather than written as zero.
type Wallet struct {
	Type    string `json:"type"`
	Name    string `json:"displayName"`
	Balance int64  `json:"balance"`
}

// Subscription is the paid-plan state. Both fields are strings because the
// upstream reports the expiry as an opaque timestamp.
type Subscription struct {
	PlanName string
	Status   string
	ExpireAt string
}

// Usage is the normalised balance view stored on the account.
type Usage struct {
	Available    int64
	Unit         string
	Wallets      []Wallet
	Subscription *Subscription
	// SubscriptionError records a failed subscription call without hiding the
	// balance (the two endpoints fail independently upstream).
	SubscriptionError string
	At                time.Time
}

// The five wallet buckets, in the order the official client shows them.
var walletLabels = []struct{ key, label string }{
	{"daily_points", "每日积分"},
	{"monthly_points", "每月会员积分"},
	{"reward_points", "奖励积分"},
	{"topup_points", "充值积分"},
	{"enterprise_topup_points", "企业加油包积分"},
}

// Balance reads the credit wallets of one credential.
func (c *Client) Balance(ctx context.Context, cred Credential) (*Usage, error) {
	resp, err := c.Do(ctx, http.MethodGet, c.origin()+BalancePath, authHeaders(cred.Token, IAMOrg(cred)), nil)
	if err != nil {
		return nil, fmt.Errorf("小浣熊积分查询失败: %w", err)
	}
	if resp.TokenExpired() {
		return nil, ErrNeedsLogin
	}
	if !resp.OK() {
		return nil, fmt.Errorf("小浣熊积分查询失败（HTTP %d）: %s", resp.Status, resp.Message)
	}
	points := resp.Data
	if points == nil {
		return nil, fmt.Errorf("小浣熊积分余额响应结构异常")
	}
	out := &Usage{Unit: "积分", At: time.Now()}
	if v, ok := intValue(points["total_chargeable_points"]); ok {
		out.Available = v
	} else if v, ok := intValue(points["available_points"]); ok {
		out.Available = v
	}
	for _, w := range walletLabels {
		if v, ok := intValue(points[w.key]); ok {
			out.Wallets = append(out.Wallets, Wallet{Type: w.key, Name: w.label, Balance: v})
		}
	}
	if sub, err := c.subscription(ctx, cred); err != nil {
		out.SubscriptionError = err.Error()
	} else {
		out.Subscription = sub
	}
	return out, nil
}

func (c *Client) subscription(ctx context.Context, cred Credential) (*Subscription, error) {
	resp, err := c.Do(ctx, http.MethodGet, c.origin()+EntitlementPath, authHeaders(cred.Token, IAMOrg(cred)), nil)
	if err != nil {
		return nil, fmt.Errorf("小浣熊订阅查询失败: %w", err)
	}
	if resp.TokenExpired() {
		return nil, ErrNeedsLogin
	}
	if !resp.OK() {
		return nil, fmt.Errorf("小浣熊订阅查询失败（HTTP %d）: %s", resp.Status, resp.Message)
	}
	data := resp.Data
	if data == nil {
		return nil, nil
	}
	// Some responses wrap the entitlement under "office".
	if office, ok := data["office"].(map[string]any); ok {
		data = office
	}
	plan := stringOf(data, "plan_type")
	if active, ok := data["active_plan"].(map[string]any); ok {
		if t := stringOf(active, "type"); t != "" {
			plan = t
		}
		if plan == "" {
			plan = stringOf(active, "plan_type")
		}
		if exp := stringOf(active, "subscription_expired_at"); exp != "" {
			return &Subscription{PlanName: normalisePlan(plan), Status: statusOf(data), ExpireAt: exp}, nil
		}
	}
	expire := firstNonEmpty(stringOf(data, "pro_expired_time"), stringOf(data, "expired_at"))
	sub := &Subscription{PlanName: normalisePlan(plan), Status: statusOf(data), ExpireAt: expire}
	if sub.PlanName == "" && expire == "" && sub.Status == "" {
		return nil, nil
	}
	return sub, nil
}

// normalisePlan maps the upstream's "no plan" spelling to an empty name so the
// panel does not render a plan called "none".
func normalisePlan(plan string) string {
	switch strings.ToLower(strings.TrimSpace(plan)) {
	case "", "none", "null":
		return ""
	}
	return plan
}

func statusOf(data map[string]any) string {
	if pro, ok := data["pro_enable"].(bool); ok && pro {
		return "active"
	}
	return "none"
}

// intValue floors a JSON number (or a numeric string) the way the upstream
// client does. Non-finite or unparsable values are "unknown", not zero.
func intValue(v any) (int64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		if err != nil || f != f {
			return 0, false
		}
		return int64(f), true
	case float64:
		if t != t {
			return 0, false
		}
		return int64(t), true
	case int64:
		return t, true
	case int:
		return int64(t), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, false
		}
		return int64(f), true
	}
	return 0, false
}
