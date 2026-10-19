package ops

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
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
	one := e.c.growthEvent("expert", 0, "u-1")
	two := e.c.growthEvent("expert", 1, "u-1")
	if one.Get("eventCode") != "expert_actual_use" || one.Get("userId") != "u-1" {
		t.Fatalf("event %v", one)
	}
	if one.Get("id") == two.Get("id") {
		t.Fatal("expert events must not repeat the same id within a batch")
	}
	chat := e.c.growthEvent("chat", 0, "u-1")
	if chat.Get("eventCode") != "chat_request_send" || chat.Get("mode") != "craft" {
		t.Fatalf("chat event %v", chat)
	}
}
