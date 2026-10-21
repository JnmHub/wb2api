package ops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"wb2api/internal/jsonx"
	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/upstream/identity"
)

// CN growth-centre tasks, cat travel and the streak keeper. Port of legacy
// wb_tasks.py. Every upstream call uses the account's own transport and
// desktop identity; event reports must carry userId or the upstream silently
// drops them. Writes are spaced at least one second apart on purpose: a burst
// of growth events is exactly what risk control looks for.

const (
	growthTasksPath   = "/v2/activity/growth/tasks"
	growthAcceptPath  = "/v2/activity/growth/tasks/accept"
	growthClaimPath   = "/activity/growth/tasks/%s/claim"
	growthEnergyPath  = "/v2/activity/growth/energy"
	growthStreakPath  = "/activity/growth/streak"
	growthHeatmapPath = "/activity/growth/heatmap"
	growthLotteryPath = "/activity/growth/lottery/summary"
	growthDrawPath    = "/activity/growth/lottery/draw"
	growthRedeemPath  = "/activity/growth/redeem"
	growthMakeupPath  = "/activity/growth/makeup-cards/use"
	travelStatusPath  = "/activity/growth/buddy/travel/status"
	travelConfigPath  = "/activity/growth/buddy/travel/config"
	travelDepartPath  = "/activity/growth/buddy/travel/depart"
	travelClaimPath   = "/activity/growth/buddy/travel/claim"
	reportPath        = "/v2/report"
	claimGiftPath     = "/billing/meter/claim-gift"
	claimCompPath     = "/billing/meter/claim-compensation"
)

// GrowthTask is one card of the CN growth centre.
type GrowthTask struct {
	Code         string `json:"code"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	JumpURL      string `json:"jump_url"`
	Status       string `json:"status"`
	Type         string `json:"type"`
	Current      int    `json:"current"`
	Target       int    `json:"target"`
	RewardCredit int    `json:"reward_credit"`
	RewardEnergy int    `json:"reward_energy"`
}

type taskSpec struct {
	kind     string
	target   int
	reward   int
	desktop  bool
	deskHint string
	unforge  bool
	reason   string
}

var growthSpecs = map[string]taskSpec{
	"create_canvas":               {kind: "canvas", target: 1, reward: 300},
	"template_5":                  {kind: "template", target: 5, reward: 200},
	"expert_5":                    {kind: "expert", target: 5, reward: 200},
	"Expert_team_use_3":           {kind: "team", target: 3, reward: 150},
	"skill_1":                     {kind: "skill", target: 1, reward: 100},
	"automation_1":                {kind: "automation", target: 1, reward: 100},
	"playbook_prompt":             {kind: "playbook", target: 1, reward: 100},
	"Expert_lighthouse":           {kind: "lighthouse", target: 1, reward: 100},
	"WorkBuddy_Valorant":          {kind: "valorant", target: 1, reward: 100},
	"Buddy_App":                   {kind: "buddy5", target: 1, reward: 100, desktop: true, deskHint: "在桌面端左上角「发现应用」进入任意一个 Buddy 应用"},
	"Buddy_App_QQ":                {kind: "buddy5", target: 1, reward: 100, desktop: true, deskHint: "在桌面端「发现应用」进入「企鹅教师助手」"},
	"Hp_Appearance":               {kind: "skin", target: 1, reward: 100},
	"chat_5":                      {kind: "chat", target: 5, reward: 100},
	"Model_chat_GLM5.2":           {kind: "glmchat", target: 1, reward: 100},
	"black_cat":                   {kind: "cat", target: 3, reward: 100},
	"RichMeow_Chat":               {kind: "richmeow", target: 1, reward: 100, desktop: true, deskHint: "在桌面端发起 1 次对话"},
	"Library_read":                {kind: "library", target: 1, reward: 100, desktop: true, deskHint: "在桌面端打开「资料库」并读完介绍文档"},
	"first_buddy":                 {kind: "chat", target: 1, reward: 300},
	"wb_wechat_oa_subscribe_task": {unforge: true, reason: "需在微信关注「腾讯 WorkBuddy」公众号满 24 小时", reward: 100},
	"Expert_Philanthropy":         {unforge: true, reason: "真实捐款动作", reward: 0},
}

type expert struct{ ID, Name string }

var expertPool = []expert{
	{"ex_PZw8Gu81HfN4", "运维工程师"}, {"ex_ROsDtJbzADFV", "产品经理"},
	{"ex_SMUnl0nJbPix", "UI设计师"}, {"ex_ZTR062oVBOCW", "数据分析师"},
	{"ex_a3sSSFBy8qaC", "后端架构师"}, {"ex_aG1kvKbq8lPx", "文案策划"},
	{"ex_al1vxtUOYQ10", "测试专家"}, {"ex_cZfiyuET9UQP", "安全顾问"},
	{"ex_eggOvQuVP0hq", "算法工程师"}, {"ex_hSwsQjkSKnkX", "前端工程师"},
	{"ex_mMbwwmFA9n9P", "项目管理专家"}, {"ex_uAQE5POfk7Zh", "增长运营专家"},
	{"ex_uZzSAScSy7FZ", "行业研究员"}, {"ex_LHywGrZOtG7G", "数据分析师"},
	{"ex_NX5C8GBciVed", "测试架构师"}, {"ex_DdCsaoq4AtcO", "云端运维专家"},
	{"ex_KzqKQguubrNQ", "内容创作专家"}, {"ex_2cvvUZQhDyeJ", "腾讯轻量云专家"},
}

var teamPool = []expert{
	{"CloudOpsTeam", "运维专家团队"}, {"CloudContentTeam", "内容专家团队"},
	{"CloudDevTeam", "研发专家团队"}, {"ProductStrategyTeam", "产品战略团队"},
	{"MarketingCampaignTeam", "营销活动团队"}, {"SalesBattleTeam", "销售作战团队"},
	{"DesignEngineTeam", "设计引擎团队"}, {"HrOperationsTeam", "人力运营团队"},
}

// pause sleeps unless the test hook replaced it; every write in this file is
// separated by at least one second because bursts trip upstream risk control.
func (c *Client) pause(d time.Duration) {
	if c.Pause != nil {
		c.Pause(d)
		return
	}
	if d > 0 {
		time.Sleep(d)
	}
}

func (c *Client) growthURL(acc *pool.Account, path string) string {
	return c.chatBase(acc.Realm) + path
}

// ---------------------------------------------------------------- fetch tasks

// GrowthTasks fetches the CN growth-centre task list (enterprise accounts have
// no personal growth centre and get an empty list without an upstream call).
func (c *Client) GrowthTasks(ctx context.Context, acc *pool.Account) ([]GrowthTask, error) {
	rec := acc.Record()
	if rec.Realm != realm.CN {
		return nil, errors.New("growth tasks are only for CN realm accounts")
	}
	if IsEnterprise(rec) {
		return []GrowthTask{}, nil
	}
	o, err := c.doJSON(ctx, acc, "GET", c.growthURL(acc, growthTasksPath), c.chatHeaders(rec), nil, 10*time.Second, 1)
	if err != nil {
		return nil, err
	}
	return parseGrowthTasks(o), nil
}

func parseGrowthTasks(o *jsonx.Obj) []GrowthTask {
	data, _ := o.Get("data").(*jsonx.Obj)
	if data == nil {
		return nil
	}
	raw, _ := data.Get("tasks").([]any)
	out := make([]GrowthTask, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(*jsonx.Obj)
		if !ok {
			continue
		}
		t := GrowthTask{
			Code:         strOf(m.Get("task_code")),
			Title:        strOf(m.Get("title")),
			Description:  firstStr2(m.Get("description"), m.Get("task_desc")),
			JumpURL:      strOf(m.Get("jump_url")),
			Status:       strOf(m.Get("accept_status")),
			Type:         strOf(m.Get("task_type")),
			RewardCredit: intOfAny(m.Get("reward_credit")),
			RewardEnergy: intOfAny(m.Get("reward_energy")),
		}
		if t.Status == "" {
			t.Status = "not_accepted"
		}
		if p, ok := m.Get("progress").(*jsonx.Obj); ok {
			t.Current = intOfAny(p.Get("current"))
			t.Target = intOfAny(p.Get("target"))
		}
		out = append(out, t)
	}
	return out
}

func strOf(v any) string { return strings.TrimSpace(jsonx.PyStrIfTruthy(v)) }

func firstStr2(a, b any) string {
	if s := strOf(a); s != "" {
		return s
	}
	return strOf(b)
}

func intOfAny(v any) int {
	if f, ok := fnum(v); ok {
		return int(f)
	}
	return 0
}

// GrowthSummary is the panel's read-only snapshot (travel state, streak, energy).
func (c *Client) GrowthSummary(ctx context.Context, acc *pool.Account) map[string]any {
	rec := acc.Record()
	out := map[string]any{"energy": 0, "streak_days": 0, "travel": map[string]any{"state": "unknown"}}
	if rec.Realm != realm.CN {
		return out
	}
	if o, err := c.doJSON(ctx, acc, "GET", c.growthURL(acc, growthEnergyPath), c.chatHeaders(rec), nil, 10*time.Second, 1); err == nil {
		if d, _ := o.Get("data").(*jsonx.Obj); d != nil {
			out["energy"] = intOfAny(d.Get("balance"))
		}
	}
	if days, ok := c.streakDays(ctx, acc); ok {
		out["streak_days"] = days
	}
	if o, err := c.doJSON(ctx, acc, "GET", c.growthURL(acc, travelStatusPath), c.chatHeaders(rec), nil, 10*time.Second, 1); err == nil {
		if d, _ := o.Get("data").(*jsonx.Obj); d != nil {
			out["travel"] = map[string]any{
				"state":               strOf(d.Get("state")),
				"daily_limit_reached": jsonx.Truthy(d.Get("daily_limit_reached")),
			}
		}
	}
	return out
}

// ------------------------------------------------------------------ cat travel

// CatTravel checks the cat's trip and either sends it out or claims the reward.
func (c *Client) CatTravel(ctx context.Context, acc *pool.Account) Result {
	rec := acc.Record()
	if rec.Realm != realm.CN {
		return failure("猫猫旅行仅限国内版账号")
	}
	if IsEnterprise(rec) {
		return Result{OK: true, Msg: "企业版账号无成长体系，跳过", Extra: map[string]any{"action": "skip"}}
	}
	o, err := c.doJSON(ctx, acc, "GET", c.growthURL(acc, travelStatusPath), c.chatHeaders(rec), nil, 10*time.Second, 1)
	if err != nil {
		return failure("查询旅行状态失败: %v", err)
	}
	data, _ := o.Get("data").(*jsonx.Obj)
	state := "unknown"
	if data != nil {
		state = strOf(data.Get("state"))
	}
	switch state {
	case "arrived":
		credit, err := c.travelClaim(ctx, acc)
		if err != nil {
			return failure("领奖失败: %v", err)
		}
		c.FetchCredits(ctx, acc)
		return Result{OK: true, Msg: fmt.Sprintf("旅行归来领奖成功！获得 %d 积分", credit), Extra: map[string]any{"action": "claim", "credit": credit}}
	case "idle":
		if data != nil && jsonx.Truthy(data.Get("daily_limit_reached")) {
			return Result{OK: true, Msg: "猫猫今日已完成旅行，明日 00:00 刷新", Extra: map[string]any{"action": "idle"}}
		}
		name, err := c.travelDepart(ctx, acc)
		if err != nil && strings.Contains(strings.ToLower(err.Error()), "no active buddy") {
			if !c.ensureBuddy(ctx, acc) {
				return failure("还没有 Buddy：需先完成并领取「领取一只 Buddy」任务（新建任务并发起对话）")
			}
			name, err = c.travelDepart(ctx, acc)
		}
		if err != nil {
			return failure("派出旅行失败: %v", err)
		}
		return Result{OK: true, Msg: fmt.Sprintf("猫猫已出发前往「%s」，预计数小时后归来！", name), Extra: map[string]any{"action": "depart"}}
	case "traveling":
		return Result{OK: true, Msg: "猫猫正在旅行途中，请稍后再来查看！", Extra: map[string]any{"action": "traveling"}}
	default:
		return Result{OK: true, Msg: "当前状态: " + state, Extra: map[string]any{"action": state}}
	}
}

func (c *Client) travelClaim(ctx context.Context, acc *pool.Account) (int, error) {
	o, err := c.doJSON(ctx, acc, "POST", c.growthURL(acc, travelClaimPath), c.chatHeaders(acc.Record()), []byte("{}"), 15*time.Second, 1)
	if err != nil {
		return 0, err
	}
	if d, _ := o.Get("data").(*jsonx.Obj); d != nil {
		return intOfAny(d.Get("reward_credit")), nil
	}
	return 0, nil
}

func (c *Client) travelDepart(ctx context.Context, acc *pool.Account) (string, error) {
	lid := 1
	if o, err := c.doJSON(ctx, acc, "GET", c.growthURL(acc, travelConfigPath), c.chatHeaders(acc.Record()), nil, 10*time.Second, 1); err == nil {
		if d, _ := o.Get("data").(*jsonx.Obj); d != nil {
			if locs, ok := d.Get("locations").([]any); ok && len(locs) > 0 {
				if m, ok := locs[0].(*jsonx.Obj); ok {
					if v := intOfAny(m.Get("id")); v > 0 {
						lid = v
					}
				}
			}
		}
	}
	body := jsonx.NewObj()
	body.Set("location_id", jsonNumber(int64(lid)))
	raw, _ := jsonx.Marshal(body)
	o, err := c.doJSON(ctx, acc, "POST", c.growthURL(acc, travelDepartPath), c.chatHeaders(acc.Record()), raw, 15*time.Second, 1)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) {
			if b, perr := jsonx.UnmarshalObj([]byte(he.Body)); perr == nil && jsonx.Truthy(b.Get("msg")) {
				return "", errors.New(strOf(b.Get("msg")))
			}
			return "", fmt.Errorf("HTTP %d", he.Status)
		}
		return "", err
	}
	name := "目的地"
	if d, _ := o.Get("data").(*jsonx.Obj); d != nil {
		if loc, _ := d.Get("location").(*jsonx.Obj); loc != nil && strOf(loc.Get("name")) != "" {
			name = strOf(loc.Get("name"))
		}
	}
	return name, nil
}

// ensureBuddy completes the auto task that grants the first Buddy.
func (c *Client) ensureBuddy(ctx context.Context, acc *pool.Account) bool {
	tasks, err := c.GrowthTasks(ctx, acc)
	if err != nil {
		return false
	}
	task, ok := findTask(tasks, "first_buddy")
	if !ok {
		return false
	}
	if task.Status == "claimed" {
		return true
	}
	c.progressTask(ctx, acc, task, 1.0, nil)
	tasks, err = c.GrowthTasks(ctx, acc)
	if err != nil {
		return false
	}
	task, ok = findTask(tasks, "first_buddy")
	return ok && task.Status == "claimed"
}

// ------------------------------------------------------------------ streak keeper

// RunStreakBonus keeps the CN check-in streak: backfills yesterday with a
// makeup card, claims the gift and compensation, redeems unlocked 7/14/28-day
// tiers, and spends every lottery chance. It is idempotent through upstream
// state only.
func (c *Client) RunStreakBonus(ctx context.Context, acc *pool.Account) Result {
	rec := acc.Record()
	if rec.Realm != realm.CN {
		return failure("连登管家仅限国内版账号")
	}
	if rec.EnterpriseID != "" {
		return Result{OK: true, Msg: "企业版账号无成长体系，跳过"}
	}
	var logs []string
	now := c.now()
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	if score, ok := c.heatmapScore(ctx, acc, yesterday); ok && score == 0 {
		if o, err := c.doJSON(ctx, acc, "GET", c.growthURL(acc, growthStreakPath), c.chatHeaders(rec), nil, 10*time.Second, 1); err == nil {
			if d, _ := o.Get("data").(*jsonx.Obj); d != nil {
				if mc, _ := d.Get("makeup_cards").(*jsonx.Obj); mc != nil && intOfAny(mc.Get("balance")) > 0 {
					body := jsonx.NewObj()
					body.Set("target_date", yesterday)
					raw, _ := jsonx.Marshal(body)
					if _, err := c.doJSON(ctx, acc, "POST", c.growthURL(acc, growthMakeupPath), c.chatHeaders(rec), raw, 15*time.Second, 1); err == nil {
						logs = append(logs, "已消耗 1 张补签卡补签昨日 ("+yesterday+")")
					}
				}
			}
		}
	}
	for _, item := range []struct{ path, label string }{{claimGiftPath, "领取新手礼包"}, {claimCompPath, "领取活动补偿"}} {
		o, err := c.doJSON(ctx, acc, "POST", c.billingBase(realm.CN)+item.path, c.billingHeaders(rec), []byte("{}"), 15*time.Second, 1)
		if err != nil || jsonx.PyStr(o.Get("code")) != "0" {
			continue
		}
		credit := 0
		if d, _ := o.Get("data").(*jsonx.Obj); d != nil {
			credit = intOfAny(d.Get("credit"))
		}
		logs = append(logs, fmt.Sprintf("%s +%d积分", item.label, credit))
	}
	if o, err := c.doJSON(ctx, acc, "GET", c.growthURL(acc, growthStreakPath), c.chatHeaders(rec), nil, 10*time.Second, 1); err == nil {
		if d, _ := o.Get("data").(*jsonx.Obj); d != nil {
			tiers, _ := d.Get("redemption_status").(*jsonx.Obj)
			var list []any
			if tiers != nil {
				list, _ = tiers.Get("tiers").([]any)
			}
			for _, item := range list {
				m, ok := item.(*jsonx.Obj)
				if !ok {
					continue
				}
				tier := strOf(m.Get("tier"))
				status := strOf(d.Get("tier_" + tier + "_status"))
				if tier == "" || status == "" || status == "locked" || status == "claimed" {
					continue
				}
				body := jsonx.NewObj()
				body.Set("tier", tier)
				body.Set("client_token", identity.NewConversationID())
				raw, _ := jsonx.Marshal(body)
				if o2, err := c.doJSON(ctx, acc, "POST", c.growthURL(acc, growthRedeemPath), c.chatHeaders(rec), raw, 15*time.Second, 1); err == nil && jsonx.PyStr(o2.Get("code")) == "0" {
					logs = append(logs, "成功兑换连登 "+tier+" 档位")
				}
			}
		}
	}
	if o, err := c.doJSON(ctx, acc, "GET", c.growthURL(acc, growthLotteryPath), c.chatHeaders(rec), nil, 10*time.Second, 1); err == nil {
		chances := 0
		if d, _ := o.Get("data").(*jsonx.Obj); d != nil {
			chances = intOfAny(d.Get("chances"))
		}
		drawn := 0
		for i := 0; i < chances; i++ {
			body := jsonx.NewObj()
			body.Set("client_token", identity.NewConversationID())
			raw, _ := jsonx.Marshal(body)
			if o2, err := c.doJSON(ctx, acc, "POST", c.growthURL(acc, growthDrawPath), c.chatHeaders(rec), raw, 15*time.Second, 1); err != nil || jsonx.PyStr(o2.Get("code")) != "0" {
				break
			}
			drawn++
			c.pause(time.Second)
		}
		if drawn > 0 {
			logs = append(logs, fmt.Sprintf("完成成长抽奖 %d 次", drawn))
		}
	}
	c.FetchCredits(ctx, acc)
	msg := strings.Join(logs, "; ")
	if msg == "" {
		msg = "连登状态正常（无需兑换/无抽奖次数）"
	}
	return Result{OK: true, Msg: msg, Extra: map[string]any{"logs": logs}}
}

func (c *Client) heatmapScore(ctx context.Context, acc *pool.Account, day string) (int, bool) {
	o, err := c.doJSON(ctx, acc, "GET", c.growthURL(acc, growthHeatmapPath), c.chatHeaders(acc.Record()), nil, 10*time.Second, 1)
	if err != nil {
		return 0, false
	}
	d, _ := o.Get("data").(*jsonx.Obj)
	if d == nil {
		return 0, false
	}
	cells, _ := d.Get("cells").([]any)
	for _, item := range cells {
		m, ok := item.(*jsonx.Obj)
		if !ok || strOf(m.Get("date")) != day {
			continue
		}
		return intOfAny(m.Get("score")), true
	}
	return 0, true
}

// ------------------------------------------------------------------ growth run

// RunGrowthTasks runs the whole CN growth flow once: auto tasks first, then
// acceptance, then progress reports, travel, and a balance refresh. It is only
// called from the panel (never scheduled).
func (c *Client) RunGrowthTasks(ctx context.Context, acc *pool.Account) (logs []string, earned int) {
	rec := acc.Record()
	if rec.Realm != realm.CN {
		return []string{"国际版不适用国内成长任务中心"}, 0
	}
	if IsEnterprise(rec) {
		return []string{"企业版账号无成长体系，跳过"}, 0
	}
	tasks, err := c.GrowthTasks(ctx, acc)
	if err != nil || len(tasks) == 0 {
		return []string{"未能获取到任务清单，请检查网络或账号状态"}, 0
	}
	logs = append(logs, fmt.Sprintf("获取到 %d 个成长任务", len(tasks)))
	for _, t := range tasks {
		if t.Type == "auto" {
			earned += c.progressTask(ctx, acc, t, 1.0, &logs)
		}
	}
	c.acceptPending(ctx, acc, &logs)
	tasks, _ = c.GrowthTasks(ctx, acc)
	for _, t := range tasks {
		if t.Type != "auto" {
			earned += c.progressTask(ctx, acc, t, 1.0, &logs)
		}
	}
	if res := c.CatTravel(ctx, acc); res.OK && res.Extra != nil {
		if credit, ok := res.Extra["credit"].(int); ok {
			earned += credit
		}
		logs = append(logs, "猫猫日常: "+res.Msg)
	}
	c.FetchCredits(ctx, acc)
	return logs, earned
}

func (c *Client) acceptPending(ctx context.Context, acc *pool.Account, logs *[]string) {
	rec := acc.Record()
	tasks, err := c.GrowthTasks(ctx, acc)
	if err != nil {
		return
	}
	var codes []string
	for _, t := range tasks {
		if t.Status != "not_accepted" || t.Type == "auto" {
			continue
		}
		if spec, known := growthSpecs[t.Code]; known && spec.unforge {
			continue
		}
		codes = append(codes, t.Code)
	}
	for start := 0; start < len(codes); start += 20 {
		end := start + 20
		if end > len(codes) {
			end = len(codes)
		}
		body := jsonx.NewObj()
		body.Set("task_codes", stringSliceAny(codes[start:end]))
		raw, _ := jsonx.Marshal(body)
		if _, err := c.doJSON(ctx, acc, "POST", c.growthURL(acc, growthAcceptPath), c.chatHeaders(rec), raw, 15*time.Second, 1); err != nil {
			*logs = append(*logs, fmt.Sprintf("接取任务失败: %v", err))
		}
		c.pause(time.Second)
	}
}

// progressTask reports enough events for one task and claims it when it lands.
func (c *Client) progressTask(ctx context.Context, acc *pool.Account, t GrowthTask, gap float64, logs *[]string) int {
	spec, known := growthSpecs[t.Code]
	if !known {
		return 0
	}
	if spec.unforge {
		if logs != nil {
			*logs = append(*logs, fmt.Sprintf("⏭ 任务 [%s] 需真实操作完成: %s, 跳过", t.Title, spec.reason))
		}
		return 0
	}
	if t.Status == "claimed" {
		return 0
	}
	target := t.Target
	if target <= 0 {
		target = spec.target
	}
	if t.Status == "completed" || t.Current >= target {
		credit, _, ok, _ := c.claimGrowthTask(ctx, acc, t.Code)
		if ok {
			if logs != nil {
				*logs = append(*logs, fmt.Sprintf("✓ 任务 [%s] 领奖 +%d 积分", t.Title, credit))
			}
			return credit
		}
		return 0
	}
	if spec.desktop {
		if logs != nil {
			*logs = append(*logs, fmt.Sprintf("⏭ 任务 [%s] 需桌面端操作: %s", t.Title, spec.deskHint))
		}
		return 0
	}
	need := target - t.Current
	if need < 1 {
		need = 1
	}
	okReports := 0
	for i := 0; i < need; i++ {
		ev := c.growthEvent(spec.kind, t.Current+i, acc.Record().UID)
		raw, _ := jsonx.Marshal([]any{ev})
		if _, err := c.doJSON(ctx, acc, "POST", c.growthURL(acc, reportPath), c.chatHeaders(acc.Record()), raw, 15*time.Second, 1); err == nil {
			okReports++
		}
		c.pause(time.Duration(gap * float64(time.Second)))
	}
	if okReports == 0 {
		if logs != nil {
			*logs = append(*logs, fmt.Sprintf("! 任务 [%s] 部分事件上报失败 (上游拒绝), 继续尝试领奖", t.Title))
		}
	}
	c.pause(1500 * time.Millisecond)
	for i := 0; i < 6; i++ {
		tasks, err := c.GrowthTasks(ctx, acc)
		if err == nil {
			if now, found := findTask(tasks, t.Code); found && (now.Current >= target || now.Status == "completed" || now.Status == "claimed") {
				credit, _, ok, _ := c.claimGrowthTask(ctx, acc, t.Code)
				if ok && logs != nil {
					*logs = append(*logs, fmt.Sprintf("✓ 任务 [%s] 领奖 +%d 积分", t.Title, credit))
				}
				return credit
			}
		}
		c.pause(1500 * time.Millisecond)
	}
	if logs != nil {
		*logs = append(*logs, fmt.Sprintf("? 任务 [%s] 已上报但进度未达成, 领奖顺延到下次运行", t.Title))
	}
	return 0
}

func findTask(tasks []GrowthTask, code string) (GrowthTask, bool) {
	for _, t := range tasks {
		if t.Code == code {
			return t, true
		}
	}
	return GrowthTask{}, false
}

func (c *Client) claimGrowthTask(ctx context.Context, acc *pool.Account, code string) (credit, energy int, ok bool, msg string) {
	rec := acc.Record()
	path := fmt.Sprintf(growthClaimPath, code)
	o, err := c.doJSON(ctx, acc, "POST", c.growthURL(acc, path), c.chatHeaders(rec), []byte("{}"), 15*time.Second, 1)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && he.Status == 400 {
			// desktop-only claims answer 400 on the chat host but work on the web host
			if o2, err2 := c.webClaim(ctx, acc, code); err2 == nil {
				o = o2
				err = nil
			}
		}
	}
	if err != nil {
		return 0, 0, false, err.Error()
	}
	if jsonx.PyStr(o.Get("code")) != "0" {
		return 0, 0, false, firstStr2(o.Get("msg"), "code="+jsonx.PyStr(o.Get("code")))
	}
	if d, _ := o.Get("data").(*jsonx.Obj); d != nil {
		credit = intOfAny(d.Get("credit"))
		energy = intOfAny(d.Get("energy"))
	}
	return credit, energy, true, ""
}

func (c *Client) webClaim(ctx context.Context, acc *pool.Account, code string) (*jsonx.Obj, error) {
	rec := acc.Record()
	h := identity.Headers{
		identity.P("Authorization", "Bearer "+rec.AccessToken),
		identity.P("Accept", "application/json, text/plain, */*"),
		identity.P("Content-Type", "application/json"),
		identity.P("Origin", "https://www.workbuddy.cn"),
		identity.P("Referer", "https://www.workbuddy.cn/profile/growth-center"),
		identity.P("x-client-platform", "web"),
		identity.P("User-Agent", webUserAgent),
		identity.P("X-User-Id", rec.UID),
		identity.P("X-Domain", "https://www.workbuddy.cn"),
	}
	base := "https://www.workbuddy.cn"
	if c.BaseOverride != "" {
		base = strings.TrimRight(c.BaseOverride, "/")
	}
	return c.doJSON(ctx, acc, "POST", base+"/activity/growth/tasks/"+code+"/claim", h, []byte("{}"), 15*time.Second, 1)
}

// ------------------------------------------------------------------- night cat

// NightGrowth completes the black_cat task inside its 23:00–08:00 window.
func (c *Client) NightGrowth(ctx context.Context, acc *pool.Account) Result {
	rec := acc.Record()
	if rec.Realm != realm.CN {
		return failure("夜猫子任务仅限国内版账号")
	}
	if IsEnterprise(rec) {
		return Result{OK: true, Msg: "企业版账号无成长体系，跳过"}
	}
	now := c.now()
	if h := now.Hour(); h < 8 || h >= 23 {
		// in the night window
	} else {
		return Result{OK: true, Msg: fmt.Sprintf("%s 当前不在 23:00-08:00 夜间窗口, 夜猫子任务跳过 (每日 01:00 自动执行)", rec.Nickname)}
	}
	tasks, err := c.GrowthTasks(ctx, acc)
	if err != nil {
		return failure("未获取到夜猫子任务清单")
	}
	t, ok := findTask(tasks, "black_cat")
	if !ok {
		return failure("未获取到夜猫子任务清单")
	}
	if t.Status == "claimed" {
		return Result{OK: true, Msg: "夜猫子任务今日已完成"}
	}
	if t.Status == "not_accepted" {
		body := jsonx.NewObj()
		body.Set("task_codes", []any{"black_cat"})
		raw, _ := jsonx.Marshal(body)
		if _, err := c.doJSON(ctx, acc, "POST", c.growthURL(acc, growthAcceptPath), c.chatHeaders(rec), raw, 15*time.Second, 1); err != nil {
			return failure("夜猫子任务未接取, 今晚跳过: %v", err)
		}
	}
	cur := t.Current
	target := t.Target
	if target <= 0 {
		target = 3
	}
	if cur < target {
		ev := c.growthEvent("cat", cur, rec.UID)
		raw, _ := jsonx.Marshal([]any{ev})
		if _, err := c.doJSON(ctx, acc, "POST", c.growthURL(acc, reportPath), c.chatHeaders(rec), raw, 15*time.Second, 1); err != nil {
			return failure("夜猫子事件上报失败: %v", err)
		}
	}
	credit := 0
	for i := 0; i < 4; i++ {
		c.pause(4 * time.Second)
		tasks, _ = c.GrowthTasks(ctx, acc)
		if t2, ok := findTask(tasks, "black_cat"); ok {
			cur = t2.Current
			if t2.Status == "claimed" {
				break
			}
			if cur >= target {
				got, _, ok, _ := c.claimGrowthTask(ctx, acc, "black_cat")
				if ok {
					credit = got
					c.FetchCredits(ctx, acc)
					return Result{OK: true, Msg: fmt.Sprintf("夜猫子任务完成，+%d 积分", credit), Extra: map[string]any{"credit": credit}}
				}
			}
		}
	}
	return Result{OK: true, Msg: fmt.Sprintf("夜猫子任务已上报，当前进度 %d/%d", cur, target)}
}

// ------------------------------------------------------------------- events

func stringSliceAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// growthEvent builds one growth-centre event. The upstream dedupes expert
// events by (eventCode, id), so the id has to differ between events of a
// batch; callers pass cur+i as cur.
func (c *Client) growthEvent(kind string, cur int, uid string) *jsonx.Obj {
	now := c.now().UnixMilli()
	cid := fmt.Sprintf("wb-task-%d-%d", now, cur)
	o := jsonx.NewObj()
	o.Set("eventCode", "heartbeat")
	o.Set("timestamp", jsonNumber(now))
	o.Set("reportDelay", jsonNumber(0))
	o.Set("conversationId", cid)
	o.Set("requestId", cid+"-req")
	o.Set("userId", uid)
	switch kind {
	case "canvas":
		o.Set("eventCode", "wbx_design_canvas_task_create")
		o.Set("source", "summon_keyword")
		o.Set("isCustomModel", false)
		o.Set("name", "")
		o.Set("inputLength", jsonNumber(12))
		o.Set("id", fmt.Sprintf("wbx-canvas-%d", now))
		o.Set("cost", jsonNumber(0))
		o.Set("isSuccessful", true)
	case "template":
		o.Set("eventCode", "agent_task_created_with_template")
		o.Set("isCustomModel", true)
		o.Set("id", fmt.Sprint(cur))
		o.Set("name", "幻灯片")
	case "expert", "team", "lighthouse", "valorant":
		e := expertPool[cur%len(expertPool)]
		if kind == "valorant" {
			// The Valorant task only progresses for this fixed expert; using the
			// generic pool would report progress on the wrong agent.
			e = expert{"ex_xQHHD1u5Jkua", "无畏契约拼豆专家"}
		}
		if kind == "team" {
			t := teamPool[cur%len(teamPool)]
			e = t
		}
		if kind == "lighthouse" {
			e = expertPool[len(expertPool)-1]
		}
		o.Set("eventCode", "expert_actual_use")
		o.Set("mode", "CLOUD")
		o.Set("id", e.ID)
		o.Set("name", e.Name)
		o.Set("expertTitle", e.Name)
		o.Set("type", "02-Engineering")
		if kind == "team" {
			o.Set("expertType", "team")
		} else {
			o.Set("expertType", "agent")
		}
		o.Set("source", "builtin")
		o.Set("version", "1.0.2")
		o.Set("cost", jsonNumber(0))
		o.Set("characterCount", jsonNumber(12))
		o.Set("messageId", cid+"-req")
		o.Set("requestModelId", "deepseek-v4-flash")
		o.Set("requestModelName", "DeepSeek V4 Flash")
	case "skill":
		o.Set("eventCode", "skill_info")
		o.Set("skillId", "skill_2096525080079265792")
		o.Set("name", "pptx")
	case "automation":
		o.Set("eventCode", "automated_task_create_suc")
		o.Set("name", "每周工作整理")
		o.Set("type", "cron")
		o.Set("source", "manually")
		o.Set("modelId", "deepseek-v4-flash")
		o.Set("modelIsThinking", false)
		sch := jsonx.NewObj()
		sch.Set("type", "recurring")
		sch.Set("rrule", "FREQ=WEEKLY;BYDAY=FR;BYHOUR=9;BYMINUTE=0")
		o.Set("schedule", sch)
		o.Set("prompt", "每周五自动整理本周工作")
	case "playbook":
		o.Set("eventCode", "playbook_prompt_send")
		o.Set("id", "worker-ledger-freedom-dashboard")
		o.Set("name", "打工人小账本")
		o.Set("type", "other")
		o.Set("promptLength", jsonNumber(10))
		o.Set("isOfficial", jsonNumber(1))
		o.Set("source", "discover")
	case "skin":
		o.Set("eventCode", "appearance_skin_apply")
		o.Set("action", "apply")
		o.Set("source", "settings_close")
		o.Set("id", "theme-tkmw7j")
		o.Set("vipLevel", "free")
		o.Set("series", "craft")
		o.Set("type", "unknown")
		o.Set("name", "和平精英激战金秋")
	case "chat", "glmchat", "cat":
		mode := "craft"
		if kind == "cat" {
			mode = "night"
		}
		o.Set("eventCode", "chat_request_send")
		o.Set("mode", mode)
		o.Set("inputLength", jsonNumber(12))
		o.Set("isPlan", false)
		o.Set("agentName", "default")
		o.Set("agentType", "conversation")
		if kind == "chat" {
			o.Set("requestModelId", "deepseek-v4-flash")
			o.Set("requestModelName", "DeepSeek V4 Flash")
		} else {
			o.Set("requestModelId", "glm-5.2")
			o.Set("requestModelName", "GLM-5.2")
		}
	}
	return o
}

// GrowthSnapshot returns what the tasks panel shows for one account.
func (c *Client) GrowthSnapshot(ctx context.Context, acc *pool.Account) map[string]any {
	tasks, err := c.GrowthTasks(ctx, acc)
	if err != nil {
		tasks = []GrowthTask{}
	}
	return map[string]any{"tasks": tasks, "summary": c.GrowthSummary(ctx, acc)}
}
