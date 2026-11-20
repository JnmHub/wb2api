package raccoon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Beijing is the upstream's natural day boundary (UTC+8, no DST). "Today" must
// not follow the host timezone: a container in UTC would otherwise read the
// second half of the Beijing day as yesterday and report "not granted".
var Beijing = time.FixedZone("Asia/Shanghai", 8*60*60)

// TodayKey is the Beijing date key (YYYY-MM-DD).
func TodayKey(now time.Time) string { return now.In(Beijing).Format("2006-01-02") }

// Grant is one daily credit entry read from the bill ledger.
type Grant struct {
	Name     string `json:"name"`
	Points   int64  `json:"points"`
	At       string `json:"at"`
	ExpireAt any    `json:"expireAt,omitempty"`
}

// CheckinResult is the outcome of the daily points link.
//
// There is no check-in verb upstream: the daily points are a per-day idempotent
// side effect of opening the desktop "home" endpoint (setting_info). The only
// authoritative answer therefore comes from the bill ledger, and "success" is
// "today's ledger contains a daily_grant", never "the HTTP call returned 200".
type CheckinResult struct {
	Success          bool
	AlreadyCompleted bool
	Message          string
	Grants           []Grant
	GrantsError      string
	// Settings carries the upstream's grant popup/toast payload verbatim.
	Settings map[string]any
}

// Credit renders the credit line the panel shows ("+100 积分").
func (r CheckinResult) Credit() string {
	var total int64
	for _, g := range r.Grants {
		total += g.Points
	}
	if total <= 0 {
		return ""
	}
	return strconv.FormatInt(total, 10) + " 积分"
}

// ClaimDaily triggers the daily grant and verifies it against the ledger.
func (c *Client) ClaimDaily(ctx context.Context, cred Credential, now time.Time) (CheckinResult, error) {
	out := CheckinResult{}
	if cred.Token == "" {
		return out, ErrNoCredential
	}
	org := IAMOrg(cred)
	today := TodayKey(now)

	before, billsErr := c.bills(ctx, cred, today)
	if errors.Is(billsErr, ErrNeedsLogin) {
		return out, billsErr
	}
	if billsErr == nil {
		out.AlreadyCompleted = len(dailyGrants(before, today)) > 0
	}

	// The trigger. A failure here means the day's grant certainly did not
	// happen, so it is reported as an error instead of a soft message.
	headers := authHeaders(cred.Token, org)
	headers.Set("X-Raccoon-Language", "zh")
	path := SettingInfoPersonalPath
	if org != "" {
		path = SettingInfoOrgPath
	}
	resp, err := c.Do(ctx, http.MethodGet, c.origin()+path, headers, nil)
	if err != nil {
		return out, fmt.Errorf("小浣熊签到请求失败: %w", err)
	}
	if resp.TokenExpired() {
		return out, ErrNeedsLogin
	}
	if !resp.OK() {
		return out, fmt.Errorf("小浣熊签到请求失败（HTTP %d）: %s", resp.Status, resp.Message)
	}
	out.Settings = map[string]any{
		"popups": resp.Data["point_grant_popups"],
		"toast":  resp.Data["point_grant_toast"],
	}

	// Verification. A ledger failure degrades to "could not verify" -- it never
	// turns into a success claim.
	if out.AlreadyCompleted {
		out.Grants = dailyGrants(before, today)
	} else if items, err := c.bills(ctx, cred, today); err != nil {
		if errors.Is(err, ErrNeedsLogin) {
			return out, err
		}
		out.GrantsError = err.Error()
	} else {
		out.Grants = dailyGrants(items, today)
	}

	var total int64
	for _, g := range out.Grants {
		total += g.Points
	}
	out.Success = total > 0
	switch {
	case total > 0:
		out.Message = "今日积分 +" + strconv.FormatInt(total, 10)
	case out.GrantsError != "":
		out.Message = "未能核对今日入账"
	default:
		out.Message = "今日未见积分入账，尚未确认领取"
	}
	if out.GrantsError != "" {
		out.Message += "；账单核对失败：" + out.GrantsError
	}
	return out, nil
}

// bills reads up to five pages of the credit ledger, stopping as soon as a page
// reaches a day before today (the ledger is newest first).
func (c *Client) bills(ctx context.Context, cred Credential, today string) ([]map[string]any, error) {
	cursor := ""
	seen := map[string]bool{}
	var items []map[string]any
	for page := 0; page < 5; page++ {
		query := url.Values{}
		query.Set("paging.limit", "20")
		query.Set("type", "all")
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		resp, err := c.Do(ctx, http.MethodGet, c.origin()+BillsPath+"?"+query.Encode(),
			authHeaders(cred.Token, IAMOrg(cred)), nil)
		if err != nil {
			return nil, fmt.Errorf("小浣熊账单查询失败: %w", err)
		}
		if resp.TokenExpired() {
			return items, ErrNeedsLogin
		}
		if !resp.OK() {
			return nil, fmt.Errorf("小浣熊账单查询失败（HTTP %d）: %s", resp.Status, resp.Message)
		}
		list, _ := resp.Data["items"].([]any)
		reachedPreviousDay := false
		for _, raw := range list {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			items = append(items, item)
			if d := billDate(item); d != "" && d < today {
				reachedPreviousDay = true
			}
		}
		if reachedPreviousDay || len(list) == 0 {
			break
		}
		next := firstNonEmpty(str(resp.Data, "next_cursor"), str(resp.Data, "cursor"))
		hasMore, hasMoreOK := resp.Data["has_more"].(bool)
		if !hasMoreOK {
			hasMore = next != ""
		}
		if !hasMore || next == "" || seen[next] {
			break
		}
		seen[next] = true
		cursor = next
	}
	return items, nil
}

// dailyGrants filters the ledger down to today's daily grant entries.
func dailyGrants(items []map[string]any, today string) []Grant {
	var out []Grant
	for _, item := range items {
		bizType := str(item, "biz_type")
		name := str(item, "event_name")
		daily := bizType == "daily_grant" || (bizType == "" && name == "每日积分发放")
		points, ok := intValue(item["points"])
		if !daily || !ok || points <= 0 || billDate(item) != today {
			continue
		}
		at := str(item, "created_at")
		if len(at) >= 19 {
			at = at[11:19]
		}
		out = append(out, Grant{Name: name, Points: points, At: at, ExpireAt: item["expire_at"]})
	}
	return out
}

// billDate extracts the Beijing date of one ledger row.
func billDate(item map[string]any) string {
	raw := firstNonEmpty(str(item, "grant_at"), str(item, "created_at"))
	if raw == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return TodayKey(t)
	}
	if len(raw) >= 10 {
		return raw[:10]
	}
	return ""
}
