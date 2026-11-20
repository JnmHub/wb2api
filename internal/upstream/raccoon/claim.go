package raccoon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// SignupTask is one one-off first-login grant.
type SignupTask struct {
	// Key is the ledger key (also what the account file stores).
	Key string
	// Title is the panel label.
	Title string
	// Path is the upstream endpoint, relative to Origin().
	Path string
	// Mobile selects the phone client identity headers.
	Mobile bool
	// NominalPoints is the advertised value; the real one comes from the
	// response and this is only a fallback.
	NominalPoints int64
}

// NominalPoints is the advertised value of each task (3000 each upstream).
const SignupTaskPoints = 3000

// SignupTasks are the two one-off grants. Order is the panel order.
var SignupTasks = []SignupTask{
	{Key: "desktop_login_grant", Title: "首次电脑端登录奖励", Path: DesktopGrantPath, NominalPoints: SignupTaskPoints},
	{Key: "mobile_login_grant", Title: "首次手机端登录奖励", Path: MobileGrantPath, Mobile: true, NominalPoints: SignupTaskPoints},
}

// ClaimOutcome is the result of probing one task.
type ClaimOutcome struct {
	Key     string `json:"key"`
	Title   string `json:"title"`
	Granted bool   `json:"granted"`
	Points  int64  `json:"points"`
	Err     string `json:"error,omitempty"`
}

// ClaimSignup probes the tasks that are not in the settlement ledger yet.
//
// The upstream has no "have I claimed this" read endpoint, so the ledger is the
// only source of truth: a settled key is not even requested. A probe that
// answers granted=false means the account already claimed it elsewhere, and it
// is still recorded as settled -- otherwise every run would ask again.
func (c *Client) ClaimSignup(ctx context.Context, cred Credential, settled map[string]int64) ([]ClaimOutcome, error) {
	if cred.Token == "" {
		return nil, ErrNoCredential
	}
	org := IAMOrg(cred)
	out := make([]ClaimOutcome, 0, len(SignupTasks))
	for _, task := range SignupTasks {
		if settled != nil && settled[task.Key] > 0 {
			continue
		}
		platform := "desktop"
		if task.Mobile {
			platform = "mobile"
		}
		headers := authHeaders(cred.Token, org)
		headers = append(headers, clientHeaders(platform)...)
		// The grant endpoints take no body; sending one is rejected.
		resp, err := c.Do(ctx, http.MethodPost, c.origin()+task.Path, headers, nil)
		if err != nil {
			return out, fmt.Errorf("小浣熊新手奖励请求失败: %w", err)
		}
		if resp.TokenExpired() {
			return out, ErrNeedsLogin
		}
		outcome := ClaimOutcome{Key: task.Key, Title: task.Title}
		if !resp.OK() {
			// A per-task failure is recorded and the next task still runs.
			outcome.Err = fmt.Sprintf("HTTP %d: %s", resp.Status, resp.Message)
			out = append(out, outcome)
			continue
		}
		granted, ok := resp.Data["granted"].(bool)
		if !ok {
			outcome.Err = "登录奖励响应缺少 granted，尚未确认领取"
			out = append(out, outcome)
			continue
		}
		outcome.Granted = granted
		if granted {
			outcome.Points = task.NominalPoints
			if popup, ok := resp.Data["popup"].(map[string]any); ok {
				if points, ok := intValue(popup["points"]); ok && points > 0 {
					outcome.Points = points
				}
			}
		}
		out = append(out, outcome)
	}
	return out, nil
}

// ErrNoSignupTasks reports that every task is already settled.
var ErrNoSignupTasks = errors.New("小浣熊新手任务已全部结算")

// SettleOutcomes merges outcomes into a settlement ledger. A task with a
// transport error is not settled (it may be retried); "granted=false" is.
func SettleOutcomes(ledger map[string]int64, outcomes []ClaimOutcome, now time.Time) map[string]int64 {
	next := make(map[string]int64, len(ledger)+len(outcomes))
	for k, v := range ledger {
		next[k] = v
	}
	ms := now.UnixMilli()
	for _, o := range outcomes {
		if o.Err != "" {
			continue
		}
		next[o.Key] = ms
	}
	return next
}
