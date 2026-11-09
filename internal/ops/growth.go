package ops

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"wb2api/internal/fingerprint"
	"wb2api/internal/jsonx"
	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
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
	// expert pins the agent a task has to be reported against (the upstream
	// dedupes by (eventCode, id) and only counts the agent the task names).
	expert *expert
}

// growthSpecs is the port of TASK_SPECS in legacy wb_tasks.py. A code missing
// here is still attempted when the task names an expert (see
// extractExpertFromTask); everything else has no event model and is skipped.
var growthSpecs = map[string]taskSpec{
	"create_canvas":                 {kind: "canvas", target: 1, reward: 300},
	"template_5":                    {kind: "template", target: 5, reward: 200},
	"expert_5":                      {kind: "expert", target: 5, reward: 200},
	"Expert_team_use_3":             {kind: "team", target: 3, reward: 150},
	"skill_1":                       {kind: "skill", target: 1, reward: 100},
	"automation_1":                  {kind: "automation", target: 1, reward: 100},
	"playbook_prompt":               {kind: "playbook", target: 1, reward: 100},
	"Expert_lighthouse":             {kind: "lighthouse", target: 1, reward: 100},
	"WorkBuddy_Valorant":            {kind: "expert", target: 1, reward: 100, expert: &expert{"ex_xQHHD1u5Jkua", "无畏契约拼豆专家"}},
	"workstation_expert":            {kind: "expert", target: 1, reward: 100, expert: &expert{"WorkspaceBuilder", "工作台搭建师"}},
	"expert_use_maidehao":           {kind: "expert", target: 1, reward: 100, expert: &expert{"ex_GDl31WahyIH0", "中国电商运营专家"}},
	"expert_use_douyinyin":          {kind: "expert", target: 1, reward: 100, expert: &expert{"ex_FbY5nlWTqDod", "抖音策略师"}},
	"expert_use_shangjia_xuanchuan": {kind: "expert", target: 1, reward: 100, expert: &expert{"ex_Weo3x2EZuSDm", "商家宣传页与海报设计师"}},
	"expert_use_bowangwang":         {kind: "expert", target: 1, reward: 100, expert: &expert{"ex_WZY136wV5aMt", "直播电商教练"}},
	// PC 灵感功能就是灵感案例（playbook）的 PC 版埋点。
	"Sequential_Tasks_7": {kind: "playbook", target: 1, reward: 100},
	"Buddy_App":          {kind: "buddy5", target: 1, reward: 100, desktop: true, deskHint: "在桌面端左上角「发现应用」进入任意一个 Buddy 应用"},
	"Buddy_App_QQ":       {kind: "buddy5", target: 1, reward: 100, desktop: true, deskHint: "在桌面端「发现应用」进入「企鹅教师助手」"},
	"Hp_Appearance":      {kind: "skin", target: 1, reward: 100},
	"chat_5":             {kind: "chat", target: 5, reward: 100},
	"Model_chat_GLM5.2":  {kind: "glmchat", target: 1, reward: 100},
	"black_cat":          {kind: "cat", target: 3, reward: 100},
	// 桌面对话事件链 / 资料库都只是**桌面端埋点**，不是人工操作：上游认的是
	// 6 连成功对话事件与 web_element_click，伪造这两组事件就能拿分（legacy
	// 实测）。标记成 desktop 跳过等于白丢 200 积分。
	"RichMeow_Chat":               {kind: "richmeow", target: 1, reward: 100},
	"Library_read":                {kind: "library", target: 1, reward: 100},
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

// RunGrowthTasks runs the whole CN growth flow once, in the same order as
// legacy run_growth_tasks: auto tasks first (first_buddy is the prerequisite of
// everything else), acceptance, the single tasks, the mini-program chain, cat
// travel with the Buddy blind boxes, and finally a balance refresh. It is only
// called from the panel (never scheduled).
func (c *Client) RunGrowthTasks(ctx context.Context, acc *pool.Account) (logs []string, earned int) {
	rec := acc.Record()
	if rec.Realm != realm.CN {
		return []string{"国际版不适用国内成长任务中心"}, 0
	}
	if IsEnterprise(rec) {
		return []string{"企业版账号无成长体系，跳过"}, 0
	}
	name := rec.Nickname
	if name == "" {
		name = short(rec.UID)
	}
	logs = append(logs, fmt.Sprintf("开始为账号 %s 运行成长任务自动化...", name))
	tasks, err := c.GrowthTasks(ctx, acc)
	if err != nil || len(tasks) == 0 {
		logs = append(logs, "未能获取到任务清单，请检查网络或账号状态")
		return logs, 0
	}
	logs = append(logs, fmt.Sprintf("获取到 %d 个成长任务", len(tasks)))

	// 0. auto 类任务不需要接取（first_buddy 的奖励就是 Buddy 实例），而且
	//    single 任务与猫猫旅行都以它为前置，所以必须最先点亮。
	for _, t := range tasks {
		if t.Type == "auto" {
			earned += c.progressTask(ctx, acc, t, 1.0, &logs)
		}
	}

	// 1. 批量接取，并复核补接一次 —— 未接取的任务上报事件不会计入进度。
	c.acceptPending(ctx, acc, &logs)
	if fresh, err := c.GrowthTasks(ctx, acc); err == nil {
		tasks = fresh
	}

	// 2. 逐个点亮
	for _, t := range tasks {
		if t.Type == "auto" {
			continue
		}
		earned += c.progressTask(ctx, acc, t, 1.0, &logs)
	}

	// 2b. 微信小程序链式成长任务（Sequential_Tasks 系列与校园日活动）
	mpCredit, mpLogs := c.RunMiniProgramTasks(ctx, acc, 1.0)
	earned += mpCredit
	logs = append(logs, mpLogs...)

	// 3. 猫猫旅行 → Buddy 盲盒 → 归来领奖后再出发
	travel := c.CatTravel(ctx, acc)
	if travel.Msg != "" {
		logs = append(logs, "猫猫日常: "+travel.Msg)
	}
	if credit, ok := extraInt(travel.Extra, "credit"); ok {
		earned += credit
	}
	if extraStr(travel.Extra, "action") == "claim" {
		if opened, msg := c.OpenBuddyBoxes(ctx, acc); opened > 0 {
			logs = append(logs, "Buddy 盲盒: "+msg)
		}
		c.pause(time.Second)
		if again := c.CatTravel(ctx, acc); extraStr(again.Extra, "action") == "depart" {
			logs = append(logs, "猫猫日常: "+again.Msg)
		}
	} else if opened, msg := c.OpenBuddyBoxes(ctx, acc); opened > 0 {
		logs = append(logs, "Buddy 盲盒: "+msg)
	}

	c.FetchCredits(ctx, acc)
	logs = append(logs, fmt.Sprintf("🎉 全部完成！本次累计新增到账: +%d 积分，当前总剩余: %s 积分", earned, creditRemain(acc.Record())))
	return logs, earned
}

func creditRemain(rec *accounts.Record) string {
	if rec == nil || rec.Credits == nil {
		return "0"
	}
	if v, ok := rec.Credits["remain"]; ok {
		return jsonx.PyStr(v)
	}
	return "0"
}

func extraInt(extra map[string]any, key string) (int, bool) {
	if extra == nil {
		return 0, false
	}
	v, ok := extra[key]
	if !ok {
		return 0, false
	}
	n, ok := v.(int)
	return n, ok
}

func extraStr(extra map[string]any, key string) string {
	if extra == nil {
		return ""
	}
	s, _ := extra[key].(string)
	return s
}

// acceptPending accepts every task that still needs it and reports what stayed
// unaccepted: progress reports sent against an unaccepted task never count.
func (c *Client) acceptPending(ctx context.Context, acc *pool.Account, logs *[]string) {
	pendingCodes := func(tasks []GrowthTask) []string {
		var codes []string
		for _, t := range tasks {
			if t.Status != "not_accepted" || t.Type == "auto" {
				continue
			}
			if spec, known := growthSpecs[t.Code]; known && (spec.unforge || spec.desktop) {
				continue
			}
			codes = append(codes, t.Code)
		}
		return codes
	}
	tasks, err := c.GrowthTasks(ctx, acc)
	if err != nil {
		return
	}
	codes := pendingCodes(tasks)
	if len(codes) == 0 {
		return
	}
	if logs != nil {
		*logs = append(*logs, fmt.Sprintf("发现 %d 个待接取任务，正在批量接取...", len(codes)))
	}
	c.acceptBatch(ctx, acc, codes, logs)
	c.pause(time.Second)
	tasks, err = c.GrowthTasks(ctx, acc)
	if err != nil {
		return
	}
	// The upstream sometimes rejects a whole batch transiently; re-check and
	// retry once, otherwise every later report lands on an unaccepted task.
	still := pendingCodes(tasks)
	if len(still) > 0 {
		if logs != nil {
			*logs = append(*logs, fmt.Sprintf("仍有 %d 个未接取，重试接取一次...", len(still)))
		}
		c.acceptBatch(ctx, acc, still, logs)
		c.pause(time.Second)
		tasks, err = c.GrowthTasks(ctx, acc)
	}
	if err == nil && logs != nil {
		if left := pendingCodes(tasks); len(left) > 0 {
			*logs = append(*logs, fmt.Sprintf("! 仍有 %d 个任务处于未接取状态，对未接取任务上报事件不会计入进度，本轮跳过这些任务", len(left)))
		}
	}
}

func (c *Client) acceptBatch(ctx context.Context, acc *pool.Account, codes []string, logs *[]string) {
	rec := acc.Record()
	accepted, rejected := 0, 0
	for start := 0; start < len(codes); start += 20 {
		end := min(start+20, len(codes))
		body := jsonx.NewObj()
		body.Set("task_codes", stringSliceAny(codes[start:end]))
		raw, _ := jsonx.Marshal(body)
		o, err := c.doJSON(ctx, acc, "POST", c.growthURL(acc, growthAcceptPath), c.chatHeaders(rec), raw, 15*time.Second, 1)
		if err != nil {
			*logs = append(*logs, fmt.Sprintf("! 接取失败: %v", err))
			rejected += end - start
			c.pause(time.Second)
			continue
		}
		if code := jsonx.PyStr(o.Get("code")); code != "0" {
			*logs = append(*logs, "! 接取被拒: "+firstStr2(o.Get("msg"), "code="+code))
			rejected += end - start
			c.pause(time.Second)
			continue
		}
		seen := map[string]bool{}
		if d, ok := o.Get("data").(*jsonx.Obj); ok {
			if arr, ok := d.Get("results").([]any); ok {
				for _, item := range arr {
					m, ok := item.(*jsonx.Obj)
					if !ok {
						continue
					}
					tc := strOf(m.Get("task_code"))
					seen[tc] = true
					if st := strOf(m.Get("status")); st == "accepted" || st == "already_accepted" {
						accepted++
						continue
					}
					rejected++
					*logs = append(*logs, fmt.Sprintf("! 任务 %s 接取未成功: %s", tc, firstStr2(m.Get("message"), m.Get("status"))))
				}
			}
		}
		for _, tc := range codes[start:end] {
			if !seen[tc] {
				rejected++
			}
		}
		c.pause(time.Second)
	}
	if accepted > 0 {
		*logs = append(*logs, fmt.Sprintf("✓ 已接取 %d 个任务", accepted))
	}
	if rejected > 0 && accepted == 0 {
		*logs = append(*logs, "! 本轮接取未成功，相关任务跳过（先解决接取失败）")
	}
}

// progressTask reports enough events for one task and claims it when it lands.
func (c *Client) progressTask(ctx context.Context, acc *pool.Account, t GrowthTask, gap float64, logs *[]string) int {
	spec, known := growthSpecs[t.Code]
	if !known {
		// 动态专家任务（expert_use_* 或 jump_url 里带 expertId）没有固定规格，
		// 用任务自己声明的专家补一个 —— 不补就会被当成未知任务静默跳过。
		if e, ok := extractExpertFromTask(t); ok {
			spec = taskSpec{kind: "expert", target: max(t.Target, 1), reward: 100, expert: &e}
			known = true
		}
	}
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
	if target <= 0 {
		target = 1
	}
	// 已完成的任务先领奖 —— 桌面端/夜间/上一次运行顺手点亮的任务都必须结算，
	// 不能因为"只能靠真实操作"就跳过丢掉奖励。
	if t.Status == "completed" || t.Current >= target {
		credit, _, ok, _ := c.claimGrowthTask(ctx, acc, t.Code)
		if ok {
			if logs != nil {
				*logs = append(*logs, fmt.Sprintf("✓ 任务 [%s] 点亮并领奖成功: +%d 积分", t.Title, credit))
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
	if t.Status == "not_accepted" && t.Type != "auto" {
		if logs != nil {
			*logs = append(*logs, fmt.Sprintf("⏭ 任务 [%s] 仍未接取, 跳过 (先解决接取失败)", t.Title))
		}
		return 0
	}
	need := target - t.Current
	if need < 1 {
		need = 1
	}
	rec := acc.Record()
	if logs != nil {
		*logs = append(*logs, fmt.Sprintf("正在点亮任务 [%s] (需上报 %d 次)...", t.Title, need))
	}
	okReports := 0
	for i := 0; i < need; i++ {
		events := c.growthEvents(spec, t.Current+i, rec.UID, rec.Nickname)
		raw, _ := jsonx.Marshal(anySlice(events))
		if _, err := c.doJSON(ctx, acc, "POST", c.growthURL(acc, reportPath), c.chatHeaders(rec), raw, 15*time.Second, 1); err == nil {
			okReports++
		}
		c.pause(time.Duration(gap * float64(time.Second)))
	}
	if okReports == 0 && logs != nil {
		*logs = append(*logs, fmt.Sprintf("! 任务 [%s] 部分事件上报失败 (上游拒绝), 继续尝试领奖", t.Title))
	}
	c.pause(1500 * time.Millisecond)

	// 等上游把进度落账再领奖。进度通常 1-3 秒可见，所以先快查几次；只有确实
	// 在动才继续等，免得每个卡住的任务都空等。
	prog := t.Current
	for attempt := 0; attempt < 6; attempt++ {
		if fresh, err := c.GrowthTasks(ctx, acc); err == nil {
			if now, found := findTask(fresh, t.Code); found {
				prog = now.Current
				if now.Status == "claimed" {
					return 0
				}
				if prog >= target || now.Status == "completed" {
					credit, _, ok, _ := c.claimGrowthTask(ctx, acc, t.Code)
					if ok && logs != nil {
						*logs = append(*logs, fmt.Sprintf("✓ 任务 [%s] 点亮并领奖成功: +%d 积分", t.Title, credit))
					}
					return credit
				}
				if prog > t.Current {
					c.pause(2500 * time.Millisecond)
					continue
				}
			}
		}
		if attempt < 2 {
			c.pause(1500 * time.Millisecond)
			continue
		}
		break
	}
	if logs != nil {
		*logs = append(*logs, fmt.Sprintf("? 任务 [%s] 已上报但进度 %d/%d 未达成, 领奖顺延到下次运行", t.Title, prog, target))
	}
	return 0
}

func anySlice(objs []*jsonx.Obj) []any {
	out := make([]any, len(objs))
	for i, o := range objs {
		out[i] = o
	}
	return out
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
	return c.claimGrowthTaskWith(ctx, acc, code, nil)
}

// claimGrowthTaskWith claims one task; extra headers let the mini-program
// 口径 (X-Client-Platform: miniprogram) claim the same task code.
func (c *Client) claimGrowthTaskWith(ctx context.Context, acc *pool.Account, code string, extra []identity.Pair) (credit, energy int, ok bool, msg string) {
	rec := acc.Record()
	path := fmt.Sprintf(growthClaimPath, code)
	h := withHeaders(c.chatHeaders(rec), extra...)
	o, err := c.doJSON(ctx, acc, "POST", c.growthURL(acc, path), h, []byte("{}"), 15*time.Second, 1)
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
		events := c.growthEvents(growthSpecs["black_cat"], cur, rec.UID, rec.Nickname)
		raw, _ := jsonx.Marshal(anySlice(events))
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

// ------------------------------------------------------------------- events

// libDocURL is the 资料库 introduction document the Library_read task counts:
// the upstream records a web_element_click on it and advances the task.
const libDocURL = "https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm"

// richMeowModel is the model pair the desktop chat chain reports. The real
// desktop client sends its own alias ("fast-model") before it is resolved.
const (
	richMeowModelID   = "fast-model"
	richMeowModelName = "fast-model"
)

// expertIDRe matches a real expert id (ex_ + 6..64 alphanumerics).
var expertIDRe = regexp.MustCompile(`^ex_[A-Za-z0-9]{6,64}$`)

// extractExpertFromTask resolves the agent a task has to be reported against.
// The jump_url deeplink wins because it carries the real id; the
// expert_use_<slug> code prefix is the fallback legacy wb_tasks.py uses. Not
// having this is why every dynamically named expert task stayed at 0/N.
func extractExpertFromTask(t GrowthTask) (expert, bool) {
	name := t.Title
	if name == "" {
		name = t.Code
	}
	if raw := strings.TrimSpace(t.JumpURL); raw != "" {
		if u, err := url.Parse(raw); err == nil {
			if d := firstQuery(u, "deeplink"); d != "" {
				if inner, err := url.Parse(d); err == nil {
					if id := firstQuery(inner, "expertId"); expertIDRe.MatchString(id) {
						return expert{id, name}, true
					}
				}
			}
			if id := firstQuery(u, "expertId"); expertIDRe.MatchString(id) {
				return expert{id, name}, true
			}
		}
	}
	if slug, ok := strings.CutPrefix(t.Code, "expert_use_"); ok && slug != "" {
		return expert{"ex_" + slug, name}, true
	}
	return expert{}, false
}

func firstQuery(u *url.URL, key string) string {
	if vs, ok := u.Query()[key]; ok && len(vs) > 0 {
		return strings.TrimSpace(vs[0])
	}
	return ""
}

// newEvent starts one growth event with the envelope every kind shares.
func (c *Client) newEvent(eventCode string, now int64, cid, uid string) *jsonx.Obj {
	o := jsonx.NewObj()
	o.Set("eventCode", eventCode)
	o.Set("timestamp", jsonNumber(now))
	o.Set("reportDelay", jsonNumber(0))
	o.Set("conversationId", cid)
	o.Set("requestId", cid+"-req")
	o.Set("userId", uid)
	return o
}

// growthEvents builds the event array one task kind needs. The upstream
// dedupes by (eventCode, id), so ids have to differ between events of a batch;
// callers pass cur+i as cur. Most kinds are a single event, but the playbook
// pair and the 6-event desktop chat chain are chains: reporting only the head
// leaves the task at 0/N forever.
func (c *Client) growthEvents(spec taskSpec, cur int, uid, nick string) []*jsonx.Obj {
	now := c.now().UnixMilli()
	cid := fmt.Sprintf("wb-task-%d-%d", now, cur)
	o := c.newEvent("heartbeat", now, cid, uid)
	switch spec.kind {
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
	case "expert", "team", "lighthouse":
		e := expertPool[cur%len(expertPool)]
		switch {
		case spec.expert != nil:
			// A task that names its agent only progresses for that agent.
			e = *spec.expert
		case spec.kind == "team":
			e = teamPool[cur%len(teamPool)]
		case spec.kind == "lighthouse":
			e = expertPool[len(expertPool)-1]
		}
		o.Set("eventCode", "expert_actual_use")
		o.Set("mode", "CLOUD")
		o.Set("id", e.ID)
		o.Set("name", e.Name)
		o.Set("expertTitle", e.Name)
		o.Set("type", "02-Engineering")
		if spec.kind == "team" {
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
	case "library":
		o.Set("eventCode", "web_element_click")
		o.Set("pageURL", libDocURL)
		o.Set("elementId", "library_doc_intro_click")
		o.Set("elementName", "WorkBuddy资料库介绍")
		o.Set("os", "Win32")
		o.Set("arch", "")
		o.Set("osVersion", "10.0")
		o.Set("userAgent", "Mozilla/5.0")
	case "playbook":
		// 官方灵感案例要"点开 CTA + 发送提示词"两条事件才算一次使用。
		cta := c.newEvent("playbook_cta_click", now, cid, uid)
		cta.Set("id", "worker-ledger-freedom-dashboard")
		cta.Set("name", "打工人小账本")
		cta.Set("type", "document")
		cta.Set("categoryId", "")
		cta.Set("categoryName", "")
		cta.Set("source", "discover")
		cta.Set("position", jsonNumber(0))
		cta.Set("messageId", cid+"-req")
		send := c.newEvent("playbook_prompt_send", now+50, cid, uid)
		send.Set("id", "worker-ledger-freedom-dashboard")
		send.Set("name", "打工人小账本")
		send.Set("type", "document")
		send.Set("categoryId", "")
		send.Set("categoryName", "")
		send.Set("promptLength", jsonNumber(30))
		send.Set("isOfficial", jsonNumber(1))
		send.Set("source", "discover")
		send.Set("messageId", cid+"-req")
		send.Set("skills", "")
		send.Set("skillNames", "")
		return []*jsonx.Obj{cta, send}
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
		if spec.kind == "cat" {
			mode = "night"
		}
		o.Set("eventCode", "chat_request_send")
		o.Set("mode", mode)
		o.Set("inputLength", jsonNumber(12))
		o.Set("isPlan", false)
		o.Set("agentName", "default")
		o.Set("agentType", "conversation")
		if spec.kind == "chat" {
			o.Set("requestModelId", "deepseek-v4-flash")
			o.Set("requestModelName", "DeepSeek V4 Flash")
		} else {
			o.Set("requestModelId", "glm-5.2")
			o.Set("requestModelName", "GLM-5.2")
		}
	case "richmeow":
		return c.richMeowEvents(now, cur, uid, nick)
	}
	return []*jsonx.Obj{o}
}

// richMeowEvents is the 6-event desktop chat chain the RichMeow_Chat
// (桌面对话事件链) task counts: 任务创建 → 发送消息 → 发起请求 → 收到消息 →
// 请求结束 → 任务完成, all under one conversation id. The upstream only
// advances the task when the whole chain arrives.
func (c *Client) richMeowEvents(now int64, idx int, uid, nick string) []*jsonx.Obj {
	cid := fmt.Sprintf("wb-richmeow-%d-%d", now, idx)
	rid := cid + "-req"
	mid := fmt.Sprintf("msg-%d-%d", now, idx)
	// The collector only attributes this chain to the desktop client when every
	// event carries the desktop envelope. Without product/ideType/platform the
	// 6 events are accepted (code 0) but the task stays at 0/1 forever — which
	// is exactly what the legacy Python chain runs into.
	env := []struct {
		k string
		v any
	}{
		{"product", "SaaS"},
		{"ideType", "WorkBuddy"},
		{"ideVersion", identity.DesktopVersionCN},
		{"extName", "workbuddy"},
		{"extVersion", identity.DesktopVersionCN},
		{"ideName", "WorkBuddy"},
		{"platform", "desktop"},
		{"os", "win32"},
		{"osVersion", "10.0"},
		{"arch", "x64"},
		{"machineId", fingerprint.MachineID(uid)},
		{"timezone", "Asia/Shanghai"},
	}

	created := jsonx.NewObj()
	for _, e := range env {
		created.Set(e.k, e.v)
	}
	created.Set("eventCode", "agent_task_created")
	created.Set("timestamp", jsonNumber(now))
	created.Set("userId", uid)
	created.Set("userNickname", nick)
	created.Set("source", "LOCAL")
	created.Set("name", "working")
	created.Set("task_target", "local")
	created.Set("mode", "craft")
	created.Set("requestModelId", richMeowModelID)
	created.Set("requestModelName", richMeowModelName)
	created.Set("has_repo", false)
	created.Set("repo_type", "none")
	created.Set("workspace_type", "empty")
	created.Set("has_connector", false)
	created.Set("connector_types", []any{})
	created.Set("has_mention", false)
	created.Set("mention_types", []any{})
	created.Set("has_template", false)
	created.Set("action", "")
	created.Set("template_name", "")
	created.Set("has_expert", false)
	created.Set("expert_id", "")
	created.Set("expert_name", "")
	created.Set("expert_industry_id", "")
	created.Set("has_skill", false)
	created.Set("skill_names", []any{})
	created.Set("conversationId", cid)
	created.Set("messageId", mid)
	created.Set("buddyId", "")
	created.Set("buddyName", "")

	msgSend := jsonx.NewObj()
	for _, e := range env {
		msgSend.Set(e.k, e.v)
	}
	msgSend.Set("eventCode", "chat_message_send")
	msgSend.Set("timestamp", jsonNumber(now+20))
	msgSend.Set("userId", uid)
	msgSend.Set("userNickname", nick)
	msgSend.Set("messageId", mid+"-ast")
	msgSend.Set("historyCount", jsonNumber(0))
	msgSend.Set("isContextTruncated", false)
	msgSend.Set("currentStepCount", jsonNumber(1))
	msgSend.Set("traceId", rid)
	msgSend.Set("rootRequestId", rid)
	msgSend.Set("parentConversationId", cid)
	msgSend.Set("agentName", "cli")
	msgSend.Set("agentType", "main")

	reqSend := jsonx.NewObj()
	for _, e := range env {
		reqSend.Set(e.k, e.v)
	}
	reqSend.Set("eventCode", "chat_request_send")
	reqSend.Set("timestamp", jsonNumber(now+40))
	reqSend.Set("userId", uid)
	reqSend.Set("userNickname", nick)
	reqSend.Set("inputLength", jsonNumber(24))
	reqSend.Set("isPlan", false)
	reqSend.Set("isAutoExecuteTerminal", false)
	reqSend.Set("isAutoModify", false)
	reqSend.Set("codebaseEnable", false)
	reqSend.Set("maxToken", jsonNumber(0))
	reqSend.Set("maxSteps", jsonNumber(500))
	reqSend.Set("temperature", jsonNumber(0))
	reqSend.Set("maxRetries", jsonNumber(0))
	reqSend.Set("mentionContexts", []any{})
	reqSend.Set("knowledgeId", []any{})
	reqSend.Set("knowledgeName", []any{})
	reqSend.Set("codebaseId", "")
	reqSend.Set("mentionContextCount", jsonNumber(0))
	reqSend.Set("command", "")
	reqSend.Set("recommendId", "")
	reqSend.Set("skillId", "")
	reqSend.Set("skillCount", jsonNumber(0))
	reqSend.Set("totalCount", jsonNumber(0))
	reqSend.Set("traceId", rid)
	reqSend.Set("rootRequestId", rid)
	reqSend.Set("parentConversationId", cid)
	reqSend.Set("conversationId", cid)
	reqSend.Set("messageId", mid)
	reqSend.Set("agentName", "cli")
	reqSend.Set("agentType", "main")

	msgResp := jsonx.NewObj()
	for _, e := range env {
		msgResp.Set(e.k, e.v)
	}
	msgResp.Set("eventCode", "chat_message_response")
	msgResp.Set("timestamp", jsonNumber(now+60))
	msgResp.Set("userId", uid)
	msgResp.Set("userNickname", nick)
	msgResp.Set("messageId", mid+"-resp")
	msgResp.Set("conversationId", cid)
	msgResp.Set("requestId", rid)
	msgResp.Set("agentName", "cli")

	reqResp := jsonx.NewObj()
	for _, e := range env {
		reqResp.Set(e.k, e.v)
	}
	reqResp.Set("eventCode", "chat_request_response")
	reqResp.Set("timestamp", jsonNumber(now+80))
	reqResp.Set("userId", uid)
	reqResp.Set("userNickname", nick)
	reqResp.Set("conversationId", cid)
	reqResp.Set("requestId", rid)
	reqResp.Set("requestModelId", richMeowModelID)
	reqResp.Set("requestModelName", richMeowModelName)
	reqResp.Set("toolCallCount", jsonNumber(0))
	reqResp.Set("inputToken", jsonNumber(6))
	reqResp.Set("outputToken", jsonNumber(12))
	reqResp.Set("duration", jsonNumber(50))
	reqResp.Set("agentName", "cli")

	done := jsonx.NewObj()
	for _, e := range env {
		done.Set(e.k, e.v)
	}
	done.Set("eventCode", "agent_task_completed")
	done.Set("timestamp", jsonNumber(now+100))
	done.Set("userId", uid)
	done.Set("userNickname", nick)
	done.Set("conversationId", cid)
	done.Set("messageId", mid)
	done.Set("status", "success")

	return []*jsonx.Obj{created, msgSend, reqSend, msgResp, reqResp, done}
}

// GrowthSnapshot returns what the tasks panel shows for one account.
func (c *Client) GrowthSnapshot(ctx context.Context, acc *pool.Account) map[string]any {
	tasks, err := c.GrowthTasks(ctx, acc)
	if err != nil {
		tasks = []GrowthTask{}
	}
	return map[string]any{"tasks": tasks, "summary": c.GrowthSummary(ctx, acc)}
}
