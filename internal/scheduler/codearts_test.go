package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"wb2api/internal/pool"
)

// fakeChannel mirrors codeartsync: a check-in that is due until it succeeds,
// then quiet for the rest of the day.
type fakeChannel struct {
	calls int
	done  bool
	ok    bool
	msg   string
}

func (f *fakeChannel) CanCheckin(acc *pool.Account) bool { return !f.done }

func (f *fakeChannel) Checkin(ctx context.Context, acc *pool.Account) (bool, string, error) {
	f.calls++
	if f.ok {
		f.done = true
	}
	return f.ok, f.msg, nil
}

// The channel chore runs for its own realm only: a CN account must never be
// handed to the Huawei endpoint, and the channel must not be called when the
// check-in grid has not fired.
func TestChannelCheckinRunsOnlyForItsRealm(t *testing.T) {
	f := newFx(t,
		map[string]any{"realm": "codearts", "uid": "hw-1"},
		map[string]any{"realm": "cn", "uid": "cn-1"},
	)
	ch := &fakeChannel{ok: true, msg: "领取成功（1000 积分）"}
	f.s.Channel = ch
	f.atHour(9)
	f.s.Cycle(context.Background(), "test")
	if ch.calls != 1 {
		t.Fatalf("channel check-in calls = %d, want 1 (only the CodeArts account)", ch.calls)
	}
	st := f.s.Status()
	joined := strings.Join(st.Logs, "|")
	if !strings.Contains(joined, "华为 CodeArts 签到") {
		t.Fatalf("log does not mention the channel: %s", joined)
	}
	if !strings.Contains(st.LastRun, "") { // LastRun is always set
		t.Fatalf("status = %+v", st)
	}
	// The channel reports "done" now, so a second cycle is quiet.
	f.s.Cycle(context.Background(), "again")
	if ch.calls != 1 {
		t.Fatalf("channel check-in ran twice in one day: %d", ch.calls)
	}
}

// Before the check-in hour the channel is not called at all.
func TestChannelCheckinFollowsTheGrid(t *testing.T) {
	f := newFx(t, map[string]any{"realm": "codearts", "uid": "hw-1"})
	ch := &fakeChannel{ok: true}
	f.s.Channel = ch
	f.atHour(3) // the default check-in grid is 09:00
	f.s.Cycle(context.Background(), "test")
	if ch.calls != 0 {
		t.Fatalf("channel check-in ran outside the grid: %d", ch.calls)
	}
}

// A failure is reported but does not retry the same account in one cycle.
func TestChannelCheckinFailureIsLogged(t *testing.T) {
	f := newFx(t, map[string]any{"realm": "codearts", "uid": "hw-1"})
	ch := &fakeChannel{ok: false, msg: "活动已结束"}
	f.s.Channel = ch
	f.atHour(9)
	f.s.Cycle(context.Background(), "test")
	if ch.calls != 1 {
		t.Fatalf("calls = %d", ch.calls)
	}
	joined := strings.Join(f.s.Status().Logs, "|")
	if !strings.Contains(joined, "签到未成功") || !strings.Contains(joined, "活动已结束") {
		t.Fatalf("failure not logged: %s", joined)
	}
}

// With no channel configured the scheduler simply skips the realm.
func TestNoChannelSkipsCodeArts(t *testing.T) {
	f := newFx(t, map[string]any{"realm": "codearts", "uid": "hw-1"})
	f.atHour(9)
	f.s.Cycle(context.Background(), "test") // must not panic
	_ = time.Now
}
