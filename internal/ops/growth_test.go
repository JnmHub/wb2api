package ops

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"wb2api/internal/jsonx"
)

// noPause removes the deliberate anti-risk-control sleeps in tests.
func noPause(e *env) { e.c.Pause = func(time.Duration) {} }

func readBody(t *testing.T, r *http.Request) string {
	t.Helper()
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

func TestCatTravelDepartsFromIdle(t *testing.T) {
	e := newEnv(t, "cn", nil)
	noPause(e)
	var departBody string
	e.vendor.json("/activity/growth/buddy/travel/status", `{"code":0,"data":{"state":"idle","daily_limit_reached":false}}`)
	e.vendor.json("/activity/growth/buddy/travel/config", `{"data":{"locations":[{"id":7,"name":"海边"},{"id":9}]}}`)
	e.vendor.h["/activity/growth/buddy/travel/depart"] = func(w http.ResponseWriter, r *http.Request) {
		departBody = readBody(t, r)
		io.WriteString(w, `{"code":0,"data":{"location":{"name":"海边"}}}`)
	}
	res := e.c.CatTravel(context.Background(), e.acc())
	if !res.OK || res.Extra["action"] != "depart" {
		t.Fatalf("%+v", res)
	}
	if !strings.Contains(res.Msg, "海边") {
		t.Fatalf("msg %q", res.Msg)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(departBody), &body); err != nil {
		t.Fatalf("depart body %q", departBody)
	}
	if body["location_id"].(float64) != 7 {
		t.Fatalf("location id not taken from config: %v", body)
	}
}

func TestCatTravelClaimsArrived(t *testing.T) {
	e := newEnv(t, "cn", nil)
	noPause(e)
	e.vendor.json("/activity/growth/buddy/travel/status", `{"code":0,"data":{"state":"arrived"}}`)
	e.vendor.json("/activity/growth/buddy/travel/claim", `{"code":0,"data":{"reward_credit":30}}`)
	res := e.c.CatTravel(context.Background(), e.acc())
	if !res.OK || res.Extra["action"] != "claim" || res.Extra["credit"].(int) != 30 {
		t.Fatalf("%+v", res)
	}
	if e.vendor.called("/buddy/travel/claim") != 1 {
		t.Fatal("claim must be called once")
	}
}

func TestCatTravelDailyLimitIsANoOp(t *testing.T) {
	e := newEnv(t, "cn", nil)
	noPause(e)
	e.vendor.json("/activity/growth/buddy/travel/status", `{"code":0,"data":{"state":"idle","daily_limit_reached":true}}`)
	res := e.c.CatTravel(context.Background(), e.acc())
	if !res.OK || res.Extra["action"] != "idle" {
		t.Fatalf("%+v", res)
	}
	if e.vendor.called("/buddy/travel/depart") != 0 {
		t.Fatal("a used-up daily limit must not depart again")
	}
}

func TestCatTravelEnterpriseSkips(t *testing.T) {
	e := newEnv(t, "cn", map[string]any{"enterpriseId": "ent-1"})
	res := e.c.CatTravel(context.Background(), e.acc())
	if !res.OK || res.Extra["action"] != "skip" {
		t.Fatalf("%+v", res)
	}
}

func TestStreakBonusBackfillsGiftsTiersAndDraws(t *testing.T) {
	e := newEnv(t, "cn", nil)
	noPause(e)
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	e.vendor.json("/activity/growth/heatmap", `{"data":{"cells":[{"date":"`+yesterday+`","score":0}]}}`)
	e.vendor.json("/activity/growth/streak", `{"data":{"streak":{"days":7},"makeup_cards":{"balance":1},"redemption_status":{"tiers":[{"tier":"7d"}]},"tier_7d_status":"available"}}`)
	e.vendor.json("/activity/growth/makeup-cards/use", `{"code":0}`)
	e.vendor.json("/billing/meter/claim-gift", `{"code":0,"data":{"credit":100}}`)
	e.vendor.json("/billing/meter/claim-compensation", `{"code":0,"data":{"credit":50}}`)
	e.vendor.json("/activity/growth/redeem", `{"code":0}`)
	e.vendor.json("/activity/growth/lottery/summary", `{"data":{"chances":2}}`)
	e.vendor.json("/activity/growth/lottery/draw", `{"code":0}`)

	res := e.c.RunStreakBonus(context.Background(), e.acc())
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	msg := res.Msg
	for _, want := range []string{"补签昨日", "领取新手礼包", "领取活动补偿", "兑换连登 7d", "抽奖 2 次"} {
		if !strings.Contains(msg, want) {
			t.Errorf("msg %q missing %q", msg, want)
		}
	}
	if n := e.vendor.called("/lottery/draw"); n != 2 {
		t.Errorf("draws %d, want 2", n)
	}
}

func TestGrowthTasksProgressAndClaim(t *testing.T) {
	e := newEnv(t, "cn", nil)
	noPause(e)
	// The task is already completed: the flow claims it and reports no events.
	e.vendor.json("/v2/activity/growth/tasks", `{"data":{"tasks":[
	  {"task_code":"create_canvas","title":"制作画布","accept_status":"completed","task_type":"single","progress":{"current":1,"target":1}}]}}`)
	e.vendor.json("/activity/growth/tasks/create_canvas/claim", `{"code":0,"data":{"credit":300}}`)
	tasks, err := e.c.GrowthTasks(context.Background(), e.acc())
	if err != nil || len(tasks) != 1 || tasks[0].Code != "create_canvas" {
		t.Fatalf("%v %+v", err, tasks)
	}
	var logs []string
	earned := e.c.progressTask(context.Background(), e.acc(), tasks[0], 1, &logs)
	if earned != 300 {
		e.vendor.mu.Lock()
		t.Logf("calls: %v", e.vendor.calls)
		e.vendor.mu.Unlock()
		t.Fatalf("earned %d logs %v", earned, logs)
	}
	if e.vendor.called("/v2/report") != 0 {
		t.Fatal("a completed task must be claimed, not reported again")
	}
}

func TestGrowthTasksUnforgeableIsNeverReported(t *testing.T) {
	e := newEnv(t, "cn", nil)
	noPause(e)
	task := GrowthTask{Code: "wb_wechat_oa_subscribe_task", Title: "关注公众号", Status: "not_accepted", Type: "single", Current: 0, Target: 1}
	var logs []string
	if got := e.c.progressTask(context.Background(), e.acc(), task, 1, &logs); got != 0 {
		t.Fatalf("unforgeable task must not earn: %d", got)
	}
	if e.vendor.called("/v2/report") != 0 || e.vendor.called("/claim") != 0 {
		t.Fatal("an unforgeable task must never be reported or claimed")
	}
	if len(logs) == 0 || !strings.Contains(logs[0], "需真实操作") {
		t.Fatalf("logs %v", logs)
	}
}

func TestNightGrowthOutsideWindowIsANoOp(t *testing.T) {
	e := newEnv(t, "cn", nil)
	e.c.Now = func() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local) }
	res := e.c.NightGrowth(context.Background(), e.acc())
	if !res.OK || !strings.Contains(res.Msg, "23:00-08:00") {
		t.Fatalf("%+v", res)
	}
	if e.vendor.called("/growth/tasks") != 0 {
		t.Fatal("outside the window nothing may be requested")
	}
}

func TestGrowthEventShapes(t *testing.T) {
	e := newEnv(t, "cn", nil)
	one := e.c.growthEvents(growthSpecs["expert_5"], 0, "u-1", "nick")[0]
	two := e.c.growthEvents(growthSpecs["expert_5"], 1, "u-1", "nick")[0]
	if one.Get("eventCode") != "expert_actual_use" || one.Get("userId") != "u-1" {
		t.Fatalf("event %v", one)
	}
	if one.Get("id") == two.Get("id") {
		t.Fatal("expert events must not repeat the same id within a batch")
	}
	chat := e.c.growthEvents(growthSpecs["chat_5"], 0, "u-1", "nick")[0]
	if chat.Get("eventCode") != "chat_request_send" || chat.Get("mode") != "craft" {
		t.Fatalf("chat event %v", chat)
	}
	valorant := e.c.growthEvents(growthSpecs["WorkBuddy_Valorant"], 0, "u-1", "nick")[0]
	if valorant.Get("eventCode") != "expert_actual_use" || valorant.Get("id") != "ex_xQHHD1u5Jkua" {
		t.Fatalf("valorant event %v", valorant)
	}
	if _, ok := growthSpecs["WorkBuddy_Valorant"]; !ok {
		t.Fatal("WorkBuddy_Valorant is not registered")
	}
}

// The 桌面对话事件链 (RichMeow_Chat) task only advances on a complete desktop
// chat chain: 创建 → 发消息 → 发请求 → 收消息 → 请求结束 → 完成.
func TestRichMeowChainIsSixLinkedEvents(t *testing.T) {
	e := newEnv(t, "cn", nil)
	spec, ok := growthSpecs["RichMeow_Chat"]
	if !ok {
		t.Fatal("RichMeow_Chat is not registered")
	}
	if spec.desktop {
		t.Fatal("RichMeow_Chat is forgeable: legacy lights it with the event chain")
	}
	evs := e.c.growthEvents(spec, 0, "u-1", "nick")
	want := []string{
		"agent_task_created", "chat_message_send", "chat_request_send",
		"chat_message_response", "chat_request_response", "agent_task_completed",
	}
	if len(evs) != len(want) {
		t.Fatalf("%d events, want %d", len(evs), len(want))
	}
	cid := ""
	for i, ev := range evs {
		if got := jsonx.PyStr(ev.Get("eventCode")); got != want[i] {
			t.Fatalf("event %d = %s, want %s", i, got, want[i])
		}
		if ev.Get("userId") != "u-1" || ev.Get("userNickname") != "nick" {
			t.Fatalf("event %d lost the identity: %v", i, ev)
		}
		c := jsonx.PyStrIfTruthy(ev.Get("conversationId"))
		if c == "" {
			c = jsonx.PyStrIfTruthy(ev.Get("parentConversationId"))
		}
		if c == "" {
			t.Fatalf("event %d has no conversation id: %v", i, ev)
		}
		if cid == "" {
			cid = c
		} else if c != cid {
			t.Fatalf("event %d broke the chain: %s != %s", i, c, cid)
		}
	}
	// 时间戳必须单调递增，上游按顺序拼这条链。
	var prev int64
	for i, ev := range evs {
		ts := intOfAny(ev.Get("timestamp"))
		if i > 0 && int64(ts) <= prev {
			t.Fatalf("event %d timestamp %d not after %d", i, ts, prev)
		}
		prev = int64(ts)
	}
	if !strings.Contains(jsonx.PyStr(evs[5].Get("status")), "success") {
		t.Fatalf("last event %v", evs[5])
	}
	// 采集侧只把带桌面设备信封的链算作真实桌面会话：少了 ideType/platform/
	// machineId，6 条事件照收（code 0）但进度永远停在 0/1。
	for i, ev := range evs {
		if got := jsonx.PyStrIfTruthy(ev.Get("ideType")); got != "WorkBuddy" {
			t.Fatalf("event %d ideType %q", i, got)
		}
		if got := jsonx.PyStrIfTruthy(ev.Get("platform")); got != "desktop" {
			t.Fatalf("event %d platform %q", i, got)
		}
		if jsonx.PyStrIfTruthy(ev.Get("machineId")) == "" {
			t.Fatalf("event %d has no machineId", i)
		}
	}
}

func TestLibraryReadSendsTheDocumentClick(t *testing.T) {
	e := newEnv(t, "cn", nil)
	spec := growthSpecs["Library_read"]
	if spec.desktop {
		t.Fatal("Library_read is forgeable: legacy lights it with web_element_click")
	}
	evs := e.c.growthEvents(spec, 0, "u-1", "nick")
	if len(evs) != 1 {
		t.Fatalf("%d events", len(evs))
	}
	if got := jsonx.PyStr(evs[0].Get("eventCode")); got != "web_element_click" {
		t.Fatalf("eventCode %q", got)
	}
	if got := jsonx.PyStr(evs[0].Get("pageURL")); got != libDocURL {
		t.Fatalf("pageURL %q", got)
	}
	if got := jsonx.PyStr(evs[0].Get("elementId")); got != "library_doc_intro_click" {
		t.Fatalf("elementId %q", got)
	}
}

func TestPlaybookSendsBothEvents(t *testing.T) {
	e := newEnv(t, "cn", nil)
	evs := e.c.growthEvents(growthSpecs["playbook_prompt"], 0, "u-1", "nick")
	if len(evs) != 2 {
		t.Fatalf("%d events, want the CTA click plus the prompt send", len(evs))
	}
	if jsonx.PyStr(evs[0].Get("eventCode")) != "playbook_cta_click" ||
		jsonx.PyStr(evs[1].Get("eventCode")) != "playbook_prompt_send" {
		t.Fatalf("codes %s / %s",
			jsonx.PyStr(evs[0].Get("eventCode")), jsonx.PyStr(evs[1].Get("eventCode")))
	}
}

// A task code that is not in growthSpecs but names its expert in the deeplink
// still has to progress: the port of extract_expert_from_task.
func TestDynamicExpertTaskIsReported(t *testing.T) {
	e := newEnv(t, "cn", nil)
	noPause(e)
	task := GrowthTask{
		Code: "expert_use_newshop", Title: "新专家", Status: "accepted", Type: "single",
		JumpURL: "workbuddy://home?deeplink=" + url.QueryEscape("workbuddy://expert?expertId=ex_AbCd1234Xy"),
		Current: 0, Target: 1,
	}
	spec := growthSpecs[task.Code]
	if spec.kind != "" {
		t.Fatal("fixture assumes an unregistered code")
	}
	x, ok := extractExpertFromTask(task)
	if !ok || x.ID != "ex_AbCd1234Xy" {
		t.Fatalf("expert %+v ok=%v", x, ok)
	}
	e.vendor.h["/v2/report"] = func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		if !strings.Contains(body, "ex_AbCd1234Xy") || !strings.Contains(body, "expert_actual_use") {
			t.Errorf("report body %s", body)
		}
		io.WriteString(w, `{"code":0}`)
	}
	e.vendor.json("/v2/activity/growth/tasks", `{"data":{"tasks":[
	  {"task_code":"expert_use_newshop","title":"新专家","accept_status":"completed","task_type":"single","progress":{"current":1,"target":1}}]}}`)
	e.vendor.json("/activity/growth/tasks/expert_use_newshop/claim", `{"code":0,"data":{"credit":100}}`)
	var logs []string
	got := e.c.progressTask(context.Background(), e.acc(), GrowthTask{
		Code: task.Code, Title: task.Title, Status: "accepted", Type: "single", JumpURL: task.JumpURL, Target: 1,
	}, 1, &logs)
	if got != 100 {
		t.Fatalf("earned %d logs %v", got, logs)
	}
	if e.vendor.called("/v2/report") != 1 {
		t.Fatalf("reports %d, logs %v", e.vendor.called("/v2/report"), logs)
	}
}

// 未接取的任务上报事件不会计入进度：必须跳过，而不是白报。
func TestUnacceptedTaskIsSkipped(t *testing.T) {
	e := newEnv(t, "cn", nil)
	noPause(e)
	var logs []string
	got := e.c.progressTask(context.Background(), e.acc(), GrowthTask{
		Code: "chat_5", Title: "发起 5 次对话", Status: "not_accepted", Type: "single", Current: 0, Target: 5,
	}, 1, &logs)
	if got != 0 || e.vendor.called("/v2/report") != 0 {
		t.Fatalf("earned %d reports %d", got, e.vendor.called("/v2/report"))
	}
	if len(logs) == 0 || !strings.Contains(logs[0], "仍未接取") {
		t.Fatalf("logs %v", logs)
	}
}

// 小程序任务走另一套标头与事件体，不看 miniprogram 头就根本看不见它们。
func TestMiniProgramTasksUseTheMiniProgramIdentity(t *testing.T) {
	e := newEnv(t, "cn", nil)
	noPause(e)
	var sawMPAccept, sawMPReport, sawMPClaim bool
	e.vendor.h["/v2/activity/growth/tasks"] = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Client-Platform") == "miniprogram" {
			io.WriteString(w, `{"data":{"tasks":[
			  {"task_code":"Sequential_Tasks_1","title":"","accept_status":"accepted","task_type":"single","progress":{"current":0,"target":1}}]}}`)
			return
		}
		io.WriteString(w, `{"data":{"tasks":[]}}`)
	}
	e.vendor.h["/v2/activity/growth/tasks/accept"] = func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("already accepted tasks must not be accepted again: %s", readBody(t, r))
		io.WriteString(w, `{"code":0}`)
	}
	e.vendor.h["/v2/report"] = func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		if r.Header.Get("X-Client-Product") == "workbuddy-mp" &&
			r.Header.Get("X-Client-Platform") == "mp-weixin" &&
			strings.Contains(body, "codebuddy.session_id") &&
			strings.Contains(body, "workspace_type") == false &&
			strings.Contains(body, "WorkBuddy_MP") {
			sawMPReport = true
		}
		io.WriteString(w, `{"code":0}`)
	}
	e.vendor.h["/activity/growth/tasks/Sequential_Tasks_1/claim"] = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Client-Platform") == "miniprogram" {
			sawMPClaim = true
		}
		io.WriteString(w, `{"code":0,"data":{"credit":100}}`)
	}
	earned, logs := e.c.RunMiniProgramTasks(context.Background(), e.acc(), 1)
	if !sawMPReport || !sawMPClaim {
		t.Fatalf("report=%v claim=%v logs %v", sawMPReport, sawMPClaim, logs)
	}
	_ = sawMPAccept
	if earned != 100 {
		t.Fatalf("earned %d logs %v", earned, logs)
	}
}
