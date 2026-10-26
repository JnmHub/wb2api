package ops

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"wb2api/internal/activity"
	"wb2api/internal/gateway/shape"
	"wb2api/internal/jsonx"
	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/upstream"
	"wb2api/internal/upstream/identity"
)

const (
	checkinPath       = "/v2/billing/meter/daily-checkin"
	chatPath          = "/v2/chat/completions"
	defaultDailyModel = "deepseek-v4.1-flash"
	profilePath       = "/console/account"
	webUserAgent      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36 Edg/140.0.0.0"
)

func stampNow(t time.Time) string { return t.Format(localTimeLayout) }

func startsWithToday(v any, now time.Time) bool {
	s := jsonx.PyStrIfTruthy(v)
	return s != "" && strings.HasPrefix(s, now.Format("2006-01-02"))
}

// CanCheckin: a CN, non-enterprise account that has not checked in today.
func (c *Client) CanCheckin(acc *pool.Account) bool {
	rec := acc.Record()
	if rec.Realm != realm.CN || IsEnterprise(rec) {
		return false
	}
	return !startsWithToday(rec.LastCheckin, c.now())
}

// CanReportActivity: the once-a-day CN chat-activity report is still due.
func (c *Client) CanReportActivity(acc *pool.Account) bool {
	rec := acc.Record()
	if rec.Realm != realm.CN || IsEnterprise(rec) {
		return false
	}
	return !startsWithToday(rec.LastActivityReport, c.now())
}

// CanDailyChat: an INTL account whose daily chat has not run today.
func (c *Client) CanDailyChat(acc *pool.Account) bool {
	rec := acc.Record()
	return rec.Realm == realm.Intl && !startsWithToday(rec.LastDailyChat, c.now())
}

// Checkin performs the CN daily check-in.
func (c *Client) Checkin(ctx context.Context, acc *pool.Account, trigger string) Result {
	rec := acc.Record()
	if rec.Realm != realm.CN {
		return failure("checkin is only available for CN realm accounts")
	}
	if IsEnterprise(rec) {
		return failure("enterprise accounts do not support checkin")
	}
	res := c.checkinUpstream(ctx, acc)
	c.record(acc, activity.TaskCheckin, trigger, res.OK, firstNonEmpty(res.Msg, res.Error))
	return res
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (c *Client) markCheckin(acc *pool.Account) {
	stamp := stampNow(c.now())
	c.update(acc, func(r *accountsRecord) { r.LastCheckin = stamp })
}

// checkinOutcome classifies a daily-checkin answer. The backend reports a
// duplicate submission as 200 + code 10001 + "请求处理中，请勿重复操作": that is a
// rejection, not a success, so it stays retryable and must not mark the account
// as checked in. An "already checked in" answer is an idempotent success.
func checkinOutcome(status int, code, msg string) (ok, retryable bool) {
	low := strings.ToLower(msg)
	switch {
	case containsAny(low, "已签到", "今日已签", "重复签到", "already checked", "already signed"):
		return true, false
	case containsAny(low, "请求处理中", "请勿重复", "操作频繁", "处理中", "processing", "too frequent", "busy"):
		return false, true
	}
	if status/100 == 2 {
		return code == "" || code == "0", false
	}
	if status == http.StatusTooManyRequests || status >= 500 {
		return false, true
	}
	return false, false
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func (c *Client) checkinUpstream(ctx context.Context, acc *pool.Account) Result {
	rec := acc.Record()
	url := c.billingBase(realm.CN) + checkinPath
	o, err := c.doJSON(ctx, acc, "POST", url, c.billingHeaders(rec), []byte("{}"), 15*time.Second, 3)
	if err != nil {
		if he, ok := err.(*HTTPError); ok {
			code, msg := "", fmt.Sprintf("HTTP %d", he.Status)
			if body, perr := jsonx.UnmarshalObj([]byte(he.Body)); perr == nil {
				code = jsonx.PyStr(body.Get("code"))
				if m := jsonx.PyStrIfTruthy(body.Get("msg")); m != "" {
					msg = m
				}
			}
			if done, retry := checkinOutcome(he.Status, code, msg); done {
				c.markCheckin(acc)
				return Result{OK: true, Msg: msg, Code: plain(code)}
			} else if retry {
				return Result{Error: msg + "（稍后重试）"}
			}
			return Result{Error: msg}
		}
		return Result{Error: err.Error()}
	}
	code := jsonx.PyStr(first(o.Get("code"), "-1"))
	msg := orStr(o.Get("msg"), "ok")
	if done, retry := checkinOutcome(http.StatusOK, code, msg); done {
		c.markCheckin(acc)
		return Result{OK: true, Msg: msg, Code: plain(o.Get("code"))}
	} else if retry {
		return Result{Error: msg + "（稍后重试）"}
	}
	return Result{Error: orStr(msg, "签到失败")}
}

func first(a, b any) any {
	if jsonx.Truthy(a) || a != nil {
		return a
	}
	return b
}

// ------------------------------------------------------------ activity report

func (c *Client) chatEvent(uid string, idx int) *jsonx.Obj {
	now := c.now().UnixMilli()
	cid := fmt.Sprintf("wb-task-%d-%d", now, idx)
	o := jsonx.NewObj()
	o.Set("eventCode", "chat_request_send")
	o.Set("timestamp", jsonNumber(now))
	o.Set("reportDelay", jsonNumber(0))
	o.Set("mode", "craft")
	o.Set("conversationId", cid)
	o.Set("requestId", cid+"-req")
	o.Set("inputLength", jsonNumber(12))
	o.Set("requestModelId", "deepseek-v4-flash")
	o.Set("requestModelName", "DeepSeek V4 Flash")
	o.Set("isPlan", false)
	o.Set("agentName", "default")
	o.Set("agentType", "conversation")
	o.Set("userId", uid)
	return o
}

// ReportActivity sends the once-a-day CN chat-activity event that lights the
// streak. The upstream silently drops events without a userId, and wants one
// report per account per day (no high-frequency reporting — that is a risk
// signal).
func (c *Client) ReportActivity(ctx context.Context, acc *pool.Account, trigger string) Result {
	rec := acc.Record()
	if rec.Realm != realm.CN {
		return failure("activity report is only for CN realm accounts")
	}
	if IsEnterprise(rec) {
		return failure("enterprise accounts do not support activity report")
	}
	body, _ := jsonx.MarshalPy([]any{c.chatEvent(rec.UID, 0)})
	o, err := c.doJSON(ctx, acc, "POST", c.chatBase(realm.CN)+"/v2/report", c.chatHeaders(rec), body, 15*time.Second, 1)
	res := Result{}
	switch {
	case err != nil || jsonx.PyStr(o.Get("code")) != "0":
		res = failure("活跃上报被上游拒绝（未返回 code=0）")
	default:
		stamp := stampNow(c.now())
		c.update(acc, func(r *accountsRecord) { r.LastActivityReport = stamp })
		days, known := c.streakDays(ctx, acc)
		switch {
		case !known:
			res = Result{OK: true, Msg: "对话活跃上报成功（连登天数未知）"}
		case days <= 0:
			res = Result{OK: true, Msg: "对话活跃上报成功，但连登仍是 0 天（可能被上游静默丢弃）", Extra: map[string]any{"streak_days": 0}}
		default:
			res = Result{OK: true, Msg: fmt.Sprintf("对话活跃上报成功（连续打卡 %d 天）", days), Extra: map[string]any{"streak_days": days}}
		}
	}
	c.record(acc, activity.TaskReport, trigger, res.OK, firstNonEmpty(res.Msg, res.Error))
	return res
}

func (c *Client) streakDays(ctx context.Context, acc *pool.Account) (int, bool) {
	o, err := c.doJSON(ctx, acc, "GET", c.chatBase(realm.CN)+"/activity/growth/streak", c.chatHeaders(acc.Record()), nil, 10*time.Second, 1)
	if err != nil {
		return 0, false
	}
	d, _ := o.Get("data").(*jsonx.Obj)
	if d == nil {
		return 0, false
	}
	st, _ := d.Get("streak").(*jsonx.Obj)
	if st == nil {
		return 0, false
	}
	f, ok := fnum(st.Get("days"))
	return int(f), ok
}

// ---------------------------------------------------------------- daily chat

func (c *Client) dailyModel(acc *pool.Account) string {
	var cands []string
	if m := strings.TrimSpace(fmt.Sprint(c.Settings.View()["daily_chat_model"])); m != "" && m != "<nil>" {
		cands = append(cands, m)
	}
	cands = append(cands, defaultDailyModel)
	if acc.Realm == realm.Intl {
		cands = append(cands, "hy3", "hy4-preview-f", "hy4-preview", "glm-5.3-flash")
	} else {
		cands = append(cands, "hy3", "hy4-preview-f", "hy4-preview")
	}
	now := c.now()
	for _, m := range cands {
		if acc.ThrottleWait(m, now) == 0 && acc.CapUntil(m, now).IsZero() {
			return m
		}
	}
	return cands[0]
}

func gz(b []byte) []byte {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.Bytes()
}

// DailyChat is the INTL daily activity: a throwaway "Hi" chat through the
// account's outbound identity, plus (unless disabled) the web-console agent
// conversation, which is the leg that actually counts for the daily bonus.
func (c *Client) DailyChat(ctx context.Context, acc *pool.Account, trigger string) Result {
	rec := acc.Record()
	if rec.Realm != realm.Intl {
		return failure("daily chat is only for international accounts")
	}
	desktop := c.dailyChatUpstream(ctx, acc)
	if !c.Settings.DailyChatWeb() {
		if desktop.OK {
			c.stampDailyChat(acc)
		}
		c.record(acc, activity.TaskDailyChat, trigger, desktop.OK, firstNonEmpty(desktop.Msg, desktop.Error))
		return desktop
	}
	web := c.dailyChatWebUpstream(ctx, acc)
	res := Result{OK: web.OK, Msg: web.Msg, Error: web.Error}
	if res.OK {
		c.stampDailyChat(acc)
		_ = c.FetchCredits(ctx, acc)
	} else if desktop.OK {
		res.Msg = "桌面通道成功，但网页通道失败：" + res.Error
	}
	if res.Extra == nil {
		res.Extra = map[string]any{}
	}
	res.Extra["desktop_ok"] = desktop.OK
	if web.Extra != nil {
		for k, v := range web.Extra {
			res.Extra["web_"+k] = v
		}
	}
	c.record(acc, activity.TaskDailyChat, trigger, res.OK, firstNonEmpty(res.Msg, res.Error))
	return res
}

func (c *Client) stampDailyChat(acc *pool.Account) {
	stamp := stampNow(c.now())
	c.update(acc, func(r *accountsRecord) { r.LastDailyChat = stamp })
}

func (c *Client) dailyChatUpstream(ctx context.Context, acc *pool.Account) Result {
	rec := acc.Record()
	model := c.dailyModel(acc)
	product := identity.NormalizeProduct(rec.Product)
	payload := jsonx.NewObj()
	payload.Set("model", model)
	msg := jsonx.NewObj()
	msg.Set("role", "user")
	msg.Set("content", "Hi")
	payload.Set("messages", []any{msg})
	payload.Set("stream", true)
	payload.Set("max_tokens", jsonNumber(10))
	payload.Set("reasoning_effort", "none")
	body := shape.Build(payload, shape.Options{})
	hdrs := identity.ChatHeaders(identity.ChatParams{
		Params: identity.Params{Product: product, Realm: rec.Realm, UID: rec.UID, Token: rec.AccessToken,
			EnterpriseID: rec.EnterpriseID, TenantID: rec.EnterpriseID, Model: model},
		DeviceToken: c.Settings.ResolveDeviceToken(),
	})
	if product == identity.ProductCLI {
		for _, f := range identity.CLIBodyDropFields {
			body.Delete(f)
		}
		body.Set("cache_retention", identity.CLICacheRetention)
	}
	raw, _ := jsonx.Marshal(body)
	if product != identity.ProductDesktop {
		raw = gz(raw)
		hdrs.Set("Content-Encoding", identity.RequestContentEnc)
	}
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := c.Up.Wire(acc.UID, acc.Proxy()).Do(rctx, upstream.Request{
		Method: http.MethodPost, URL: c.chatBase(rec.Realm) + chatPath, Headers: hdrs, Body: raw})
	if err != nil {
		return Result{Error: err.Error()}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return failure("HTTP %d: %s", resp.StatusCode, truncate(string(data), 100))
	}
	_ = c.FetchCredits(ctx, acc)
	return Result{OK: true, Msg: "每日活跃对话成功完成"}
}

// ------------------------------------------------------------------ profile

// SyncProfile refreshes the nickname from the web console. Only uid and
// nickname are read; a uid mismatch against the credential is an error.
func (c *Client) SyncProfile(ctx context.Context, acc *pool.Account) Result {
	rec := acc.Record()
	origin := "https://www.workbuddy.ai"
	if rec.Realm == realm.CN {
		origin = "https://www.workbuddy.cn"
	}
	h := identity.Headers{
		identity.P("Authorization", "Bearer "+rec.AccessToken),
		identity.P("Accept", "application/json, text/plain, */*"),
		identity.P("x-client-platform", "web"),
		identity.P("Origin", "https://www.workbuddy.cn"),
		identity.P("Referer", "https://www.workbuddy.cn/profile/account-settings"),
		identity.P("User-Agent", webUserAgent),
	}
	base := origin
	if c.BaseOverride != "" {
		base = strings.TrimRight(c.BaseOverride, "/")
	}
	o, err := c.doJSON(ctx, acc, "GET", base+profilePath, h, nil, 15*time.Second, 1)
	if err != nil {
		return Result{Error: err.Error()}
	}
	uid := orStr(o.Get("uid"), "")
	if uid != "" && rec.UID != "" && uid != rec.UID {
		return failure("profile uid mismatch")
	}
	nick := ""
	if s, ok := o.Get("nickname").(string); ok {
		nick = strings.TrimSpace(s)
	}
	if nick != "" && nick != rec.Nickname {
		c.update(acc, func(r *accountsRecord) { r.Nickname = nick })
	}
	return Result{OK: true, Msg: nick, Extra: map[string]any{"nickname": nick}}
}
