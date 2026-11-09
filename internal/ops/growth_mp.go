package ops

import (
	"context"
	"fmt"
	"time"

	"wb2api/internal/jsonx"
	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/upstream/identity"
)

// 微信小程序成长任务（Sequential_Tasks 系列与校园日活动）与 Buddy 盲盒。
// Port of run_miniprogram_tasks / do_buddy_open in legacy wb_tasks.py.
//
// 小程序任务在同一个 /v2/activity/growth/tasks 上返回，但接取、上报、领奖都
// 要带小程序口径的标头（X-Client-Platform: miniprogram），事件体也换成小程序
// 的埋点字段（codebuddy.session_id 等）。不带这套标头时上游返回的是桌面任务，
// 小程序任务完全看不见，这就是"小程序那 100 分拿不到"的原因。

const (
	growthBoxQuotaPath = "/v2/activity/growth/buddy/quota"
	growthBoxOpenPath  = "/v2/activity/growth/buddy/open"
)

// mpHeaders make a growth call look like it came from the mini program.
var mpHeaders = []identity.Pair{
	identity.P("X-Client-Platform", "miniprogram"),
	identity.P("User-Agent", "Mozilla/5.0 (Linux; Android 14; MicroMessenger/8.0.49 WeChat/0.8.0 MiniProgramEnv/android; wkbrowser xweb)"),
}

// mpReportHeaders are the mini-program event-report identity.
var mpReportHeaders = []identity.Pair{
	identity.P("Content-Type", "application/json"),
	identity.P("Accept", "application/json"),
	identity.P("X-Client-Product", "workbuddy-mp"),
	identity.P("X-Client-Version", "2.4.0"),
	identity.P("X-Client-Platform", "mp-weixin"),
	identity.P("X-Platform", "wechatmp"),
}

type mpSpec struct {
	name   string
	target int
	reward int
}

var mpTaskSpecs = map[string]mpSpec{
	"Sequential_Tasks_1": {name: "小程序对话", target: 1, reward: 100},
	"Sequential_Tasks_2": {name: "小程序专家对话", target: 1, reward: 200},
	"Sequential_Tasks_3": {name: "小程序对话5次", target: 5, reward: 300},
	"Sequential_Tasks_4": {name: "小程序定时任务", target: 1, reward: 100},
	"Sequential_Tasks_5": {name: "小程序GLM5.2", target: 1, reward: 100},
	"Sequential_Tasks_6": {name: "小程序对话10次", target: 10, reward: 400},
	"school_season":      {name: "校园日活动", target: 1, reward: 100},
}

// withHeaders applies extra headers with dict semantics: an existing name keeps
// its position and gets the new value, a new name is appended.
func withHeaders(h identity.Headers, extra ...identity.Pair) identity.Headers {
	out := make(identity.Headers, len(h), len(h)+len(extra))
	copy(out, h)
	for _, p := range extra {
		out.Set(p.Name, p.Value)
	}
	return out
}

// GrowthTasksMP fetches the task list through the mini-program identity. The
// panel uses it to show the chain the run log reports on.
func (c *Client) GrowthTasksMP(ctx context.Context, acc *pool.Account) ([]GrowthTask, error) {
	rec := acc.Record()
	if rec.Realm != realm.CN || IsEnterprise(rec) {
		return nil, nil
	}
	o, err := c.doJSON(ctx, acc, "GET", c.growthURL(acc, growthTasksPath), withHeaders(c.chatHeaders(rec), mpHeaders...), nil, 10*time.Second, 1)
	if err != nil {
		return nil, err
	}
	tasks := parseGrowthTasks(o)
	for i := range tasks {
		if tasks[i].Title == "" {
			if spec, ok := mpTaskSpecs[tasks[i].Code]; ok {
				tasks[i].Title = spec.name
			}
		}
		if tasks[i].Target <= 0 {
			if spec, ok := mpTaskSpecs[tasks[i].Code]; ok {
				tasks[i].Target = spec.target
			}
		}
	}
	return tasks, nil
}

// IsMiniProgramTask reports whether a task code belongs to the mini-program
// chain (the panel and tools label those rows separately).
func IsMiniProgramTask(code string) bool {
	_, ok := mpTaskSpecs[code]
	return ok
}

// RunMiniProgramTasks drives the Sequential_Tasks chain and the campus-day
// event. It returns the credits it actually claimed plus the log lines.
func (c *Client) RunMiniProgramTasks(ctx context.Context, acc *pool.Account, gap float64) (earned int, logs []string) {
	rec := acc.Record()
	if rec.Realm != realm.CN || IsEnterprise(rec) {
		return 0, nil
	}
	tasks, err := c.GrowthTasksMP(ctx, acc)
	if err != nil || len(tasks) == 0 {
		return 0, nil
	}
	supported := 0
	var unaccepted []string
	for _, t := range tasks {
		if !IsMiniProgramTask(t.Code) {
			continue
		}
		supported++
		if t.Status == "not_accepted" {
			unaccepted = append(unaccepted, t.Code)
		}
	}
	if supported == 0 {
		return 0, nil
	}
	logs = append(logs, fmt.Sprintf("📱 发现 %d 个小程序成长任务，开始执行自动化...", supported))
	if len(unaccepted) > 0 {
		if accepted := c.acceptBatchMP(ctx, acc, unaccepted, &logs); accepted > 0 {
			logs = append(logs, fmt.Sprintf("✓ 已接取小程序任务: %d 个", accepted))
		}
		c.pause(time.Second)
		if fresh, err := c.GrowthTasksMP(ctx, acc); err == nil && len(fresh) > 0 {
			tasks = fresh
		}
	}

	now := c.now().UnixMilli()
	for _, t := range tasks {
		spec, ok := mpTaskSpecs[t.Code]
		if !ok {
			continue
		}
		name := t.Title
		if name == "" {
			name = spec.name
		}
		target := t.Target
		if target <= 0 {
			target = spec.target
		}
		if t.Status == "claimed" {
			continue
		}
		if t.Status == "completed" || t.Current >= target {
			if credit, _, ok, msg := c.claimGrowthTaskWith(ctx, acc, t.Code, mpHeaders); ok {
				earned += credit
				logs = append(logs, fmt.Sprintf("✓ 小程序任务 [%s] 领奖成功: +%d 积分", name, credit))
			} else {
				logs = append(logs, fmt.Sprintf("! 小程序任务 [%s] 领奖失败: %s", name, msg))
			}
			continue
		}
		if t.Status == "not_accepted" {
			// 典型是链式任务还没解锁 (上游回 "task locked until <日期>")。
			logs = append(logs, fmt.Sprintf("⏭ 小程序任务 [%s] 仍未接取 (上游锁定或未放量), 跳过", name))
			continue
		}
		need := max(target-t.Current, 1)
		var events []*jsonx.Obj
		for i := 0; i < need; i++ {
			events = append(events, c.mpEvents(t.Code, now, i, rec.UID, rec.Nickname)...)
		}
		if len(events) == 0 {
			continue
		}
		logs = append(logs, fmt.Sprintf("正在点亮小程序任务 [%s] (需上报 %d 次)...", name, need))
		if err := c.reportMPEvents(ctx, acc, events); err != nil {
			logs = append(logs, fmt.Sprintf("! 小程序任务 [%s] 事件上报失败: %v", name, err))
		}
		c.pause(1500 * time.Millisecond)
		if credit, _, ok, _ := c.claimGrowthTaskWith(ctx, acc, t.Code, mpHeaders); ok {
			earned += credit
			logs = append(logs, fmt.Sprintf("✓ 小程序任务 [%s] 点亮并领奖成功: +%d 积分", name, credit))
		} else {
			logs = append(logs, fmt.Sprintf("? 小程序任务 [%s] 已上报，领奖将在下轮重试", name))
		}
		c.pause(time.Duration(gap * float64(time.Second)))
	}
	return earned, logs
}

func (c *Client) acceptBatchMP(ctx context.Context, acc *pool.Account, codes []string, logs *[]string) int {
	rec := acc.Record()
	h := withHeaders(c.chatHeaders(rec), mpHeaders...)
	accepted := 0
	for start := 0; start < len(codes); start += 20 {
		end := min(start+20, len(codes))
		body := jsonx.NewObj()
		body.Set("task_codes", stringSliceAny(codes[start:end]))
		raw, _ := jsonx.Marshal(body)
		o, err := c.doJSON(ctx, acc, "POST", c.growthURL(acc, growthAcceptPath), h, raw, 15*time.Second, 1)
		if err != nil {
			*logs = append(*logs, fmt.Sprintf("! 小程序任务接取失败: %v", err))
			continue
		}
		if code := jsonx.PyStr(o.Get("code")); code != "0" {
			*logs = append(*logs, "! 小程序任务接取被拒: "+firstStr2(o.Get("msg"), "code="+code))
			continue
		}
		if d, ok := o.Get("data").(*jsonx.Obj); ok {
			if arr, ok := d.Get("results").([]any); ok {
				for _, item := range arr {
					m, ok := item.(*jsonx.Obj)
					if !ok {
						continue
					}
					if st := strOf(m.Get("status")); st == "accepted" || st == "already_accepted" {
						accepted++
						continue
					}
					*logs = append(*logs, fmt.Sprintf("! 小程序任务 %s 接取未成功: %s",
						strOf(m.Get("task_code")), firstStr2(m.Get("message"), m.Get("status"))))
				}
			}
		}
		c.pause(time.Second)
	}
	return accepted
}

// mpEventBase is the mini-program event envelope both the heartbeat and every
// task event carry.
func mpEventBase(now int64, uid, nick string) *jsonx.Obj {
	o := jsonx.NewObj()
	o.Set("timestamp", jsonNumber(now))
	o.Set("ideType", "WorkBuddy_MP")
	o.Set("ideVersion", "2.4.0")
	o.Set("extName", "workbuddy-mp")
	o.Set("extVersion", "2.4.0")
	o.Set("product", "SaaS")
	o.Set("ideName", "wx_app_cloud")
	o.Set("platform", "mini_program")
	o.Set("source", "mini_program")
	o.Set("os", "android")
	o.Set("osVersion", "14")
	o.Set("arch", "arm64")
	mid := uid
	if len(mid) > 16 {
		mid = mid[:16]
	}
	if mid == "" {
		mid = "guest"
	}
	o.Set("machineId", "mp-"+mid)
	o.Set("timezone", "Asia/Shanghai")
	o.Set("userId", uid)
	o.Set("userNickname", nick)
	return o
}

// mpEvents builds one mini-program task event. The chain codes all send the
// same chat_request_send fingerprint; the expert, cron and GLM variants swap in
// their own event code and fields.
func (c *Client) mpEvents(code string, now int64, i int, uid, nick string) []*jsonx.Obj {
	cid := fmt.Sprintf("wb-mp-%d-%s-%d", now, code, i)
	rid := fmt.Sprintf("req-%d-%s-%d", now, code, i)
	o := mpEventBase(now, uid, nick)
	switch code {
	case "Sequential_Tasks_1", "Sequential_Tasks_3", "Sequential_Tasks_6", "school_season":
		o.Set("eventCode", "chat_request_send")
		o.Set("mode", "chat")
		o.Set("inputLength", jsonNumber(12))
		o.Set("isPlan", false)
		o.Set("isAutoExecuteTerminal", false)
		o.Set("isAutoModify", false)
		o.Set("codebaseEnable", false)
		o.Set("maxToken", jsonNumber(0))
		o.Set("maxSteps", jsonNumber(500))
		o.Set("temperature", jsonNumber(0))
		o.Set("maxRetries", jsonNumber(0))
		o.Set("mentionContexts", []any{})
		o.Set("knowledgeId", []any{})
		o.Set("knowledgeName", []any{})
		o.Set("codebaseId", "")
		o.Set("mentionContextCount", jsonNumber(0))
		o.Set("command", "")
		o.Set("requestId", rid)
		o.Set("traceId", rid)
		o.Set("rootRequestId", rid)
		o.Set("parentConversationId", cid)
		o.Set("conversationId", cid)
		o.Set("messageId", fmt.Sprintf("msg-%d-%d", now, i))
		o.Set("agentName", "mp")
		o.Set("agentType", "main")
		o.Set("codebuddy.session_id", cid)
		o.Set("codebuddy.conversation_request_id", rid)
	case "Sequential_Tasks_2":
		o.Set("eventCode", "expert_actual_use")
		o.Set("id", "WorkspaceBuilder")
		o.Set("name", "WorkspaceBuilder")
		o.Set("expertTitle", "工作台搭建师")
		o.Set("type", "send_message")
		o.Set("characterCount", jsonNumber(12))
		o.Set("expertType", "agent")
		o.Set("extVersion", "2.2.8")
	case "Sequential_Tasks_4":
		o.Set("eventCode", "automated_task_create_suc")
		o.Set("mode", "CLOUD")
		o.Set("name", "每日任务提醒")
		o.Set("source", "manually")
		o.Set("skills", "")
		o.Set("skillCount", jsonNumber(0))
		o.Set("scheduleType", "daily")
	case "Sequential_Tasks_5":
		o.Set("eventCode", "chat_request_send")
		o.Set("mode", "chat")
		o.Set("inputLength", jsonNumber(12))
		o.Set("isPlan", false)
		o.Set("isAutoExecuteTerminal", false)
		o.Set("isAutoModify", false)
		o.Set("codebaseEnable", false)
		o.Set("maxToken", jsonNumber(0))
		o.Set("maxSteps", jsonNumber(500))
		o.Set("temperature", jsonNumber(0))
		o.Set("maxRetries", jsonNumber(0))
		o.Set("mentionContexts", []any{})
		o.Set("knowledgeId", []any{})
		o.Set("knowledgeName", []any{})
		o.Set("codebaseId", "")
		o.Set("mentionContextCount", jsonNumber(0))
		o.Set("command", "")
		o.Set("requestId", rid)
		o.Set("traceId", rid)
		o.Set("rootRequestId", rid)
		o.Set("parentConversationId", cid)
		o.Set("conversationId", cid)
		o.Set("messageId", fmt.Sprintf("msg-%d-%d", now, i))
		o.Set("agentName", "mp")
		o.Set("agentType", "main")
		o.Set("codebuddy.session_id", cid)
		o.Set("codebuddy.conversation_request_id", rid)
		o.Set("requestModelId", "glm-5.2")
		o.Set("requestModelName", "GLM-5.2")
	default:
		return nil
	}
	return []*jsonx.Obj{o}
}

func (c *Client) reportMPEvents(ctx context.Context, acc *pool.Account, events []*jsonx.Obj) error {
	rec := acc.Record()
	h := withHeaders(c.chatHeaders(rec), mpReportHeaders...)
	raw, err := jsonx.Marshal(anySlice(events))
	if err != nil {
		return err
	}
	o, err := c.doJSON(ctx, acc, "POST", c.growthURL(acc, reportPath), h, raw, 15*time.Second, 1)
	if err != nil {
		return err
	}
	if code := jsonx.PyStr(o.Get("code")); code != "0" {
		return fmt.Errorf("code=%s %s", code, firstStr2(o.Get("msg"), ""))
	}
	return nil
}

// OpenBuddyBoxes spends accumulated growth energy on Buddy blind boxes. It
// earns no credits but keeps the energy from idling (the desktop client opens
// them automatically after a travel reward lands).
func (c *Client) OpenBuddyBoxes(ctx context.Context, acc *pool.Account) (int, string) {
	rec := acc.Record()
	if rec.Realm != realm.CN || IsEnterprise(rec) {
		return 0, ""
	}
	o, err := c.doJSON(ctx, acc, "GET", c.growthURL(acc, growthBoxQuotaPath), c.chatHeaders(rec), nil, 10*time.Second, 1)
	if err != nil {
		return 0, ""
	}
	affordable, maxOpen := 0, 1
	if d, ok := o.Get("data").(*jsonx.Obj); ok && jsonx.PyStr(o.Get("code")) == "0" {
		affordable = intOfAny(d.Get("affordable"))
		if v := intOfAny(d.Get("max_open_count")); v > 0 {
			maxOpen = v
		}
	}
	if affordable <= 0 {
		return 0, ""
	}
	count := min(affordable, max(maxOpen, 1))
	body := jsonx.NewObj()
	body.Set("count", jsonNumber(int64(count)))
	body.Set("client_token", identity.NewConversationID())
	raw, _ := jsonx.Marshal(body)
	o, err = c.doJSON(ctx, acc, "POST", c.growthURL(acc, growthBoxOpenPath), c.chatHeaders(rec), raw, 15*time.Second, 1)
	if err != nil {
		return 0, ""
	}
	if code := jsonx.PyStr(o.Get("code")); code != "0" {
		return 0, "开盲盒失败: " + firstStr2(o.Get("msg"), "code="+code)
	}
	name := "新 Buddy"
	if d, ok := o.Get("data").(*jsonx.Obj); ok {
		for _, key := range []string{"buddy", "name", "buddies"} {
			if v := d.Get(key); jsonx.Truthy(v) {
				name = jsonx.PyStr(v)
				break
			}
		}
	}
	return count, fmt.Sprintf("开启 Buddy 盲盒 ×%d 成功 (%s)", count, name)
}
