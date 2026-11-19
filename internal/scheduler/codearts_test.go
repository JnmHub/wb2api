package scheduler

import (
	"context"
	"testing"
	"time"

	"wb2api/internal/codeartsync"
	"wb2api/internal/pool"
	"wb2api/internal/store/accounts"
	"wb2api/internal/store/settings"
)

type fakeChannel struct {
	pool  *pool.Pool
	now   func() time.Time
	calls chan string
}

func (f *fakeChannel) CanCheckin(acc *pool.Account) bool {
	return !codeartsync.DailyStatus(acc.Record(), f.now()).OK
}

func (f *fakeChannel) RunDaily(_ context.Context, acc *pool.Account, trigger string) (codeartsync.DailyResult, error) {
	f.pool.Update(acc.UID, func(r *accounts.Record) {
		r.LastCheckin = map[string]any{"ok": true, "state": "already_claimed", "channel": "codearts",
			"day": f.now().In(beijing).Format("2006-01-02")}
	})
	f.calls <- acc.UID + "/" + trigger
	return codeartsync.DailyResult{OK: true, State: codeartsync.DailyAlready}, nil
}

func awaitCodeArts(t *testing.T, f *fakeChannel, want string) {
	t.Helper()
	select {
	case got := <-f.calls:
		if got != want {
			t.Fatalf("call = %q, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("the CodeArts sweep did not run")
	}
}

func TestCodeArtsScheduleIsIndependentAndCatchesUp(t *testing.T) {
	f := newFx(t,
		map[string]any{"realm": "codearts", "uid": "hw-1"},
		map[string]any{"realm": "codearts", "uid": "hw-2"},
		map[string]any{"realm": "cn", "uid": "cn-1"},
	)
	now := time.Date(2026, 11, 20, 16, 5, 0, 0, time.UTC) // 00:05 in Beijing
	f.s.Now = func() time.Time { return now }
	ch := &fakeChannel{pool: f.s.Pool, now: f.s.Now, calls: make(chan string, 4)}
	f.s.Channel = ch
	uids := []string{"hw-2"}
	if err := f.s.Settings.SetCodeArtsDaily(settings.CodeArtsDailyPatch{UIDs: &uids}); err != nil {
		t.Fatal(err)
	}
	if f.s.runCodeArts(context.Background(), false) {
		t.Fatal("automatic check-in ran before its Beijing start time")
	}
	// Simulate a WorkBuddy cycle occupying its single background slot.
	f.s.running = true
	now = now.Add(time.Hour)
	if !f.s.runCodeArts(context.Background(), false) {
		t.Fatal("the independent sweep failed to start after its scheduled time")
	}
	awaitCodeArts(t, ch, "hw-2/makeup")
	st := f.s.Status().CodeArts
	if st.AccountCount != 1 || st.States[codeartsync.DailyAlready] != 1 || st.LastRun == "" {
		t.Fatalf("daily status = %+v", st)
	}
	if f.s.runCodeArts(context.Background(), false) {
		t.Fatal("the completed account was scheduled again")
	}
}

func TestCodeArtsManualRunRespectsTasksAndRestrictions(t *testing.T) {
	f := newFx(t,
		map[string]any{"realm": "codearts", "uid": "hw-1"},
		map[string]any{"realm": "codearts", "uid": "hw-2", "tasksEnabled": false},
		map[string]any{"realm": "codearts", "uid": "hw-3", "enabled": false},
		map[string]any{"realm": "codearts", "uid": "hw-4"},
		map[string]any{"realm": "cn", "uid": "cn-1"},
	)
	f.s.Pool.Get("hw-4").NoteRestricted(f.s.now(), time.Hour)
	ch := &fakeChannel{pool: f.s.Pool, now: f.s.now, calls: make(chan string, 4)}
	f.s.Channel = ch
	if err := f.s.Settings.SetSchedulerEnabled(false); err != nil {
		t.Fatal(err)
	}
	if f.s.runCodeArts(context.Background(), false) || !f.s.TriggerCodeArts() {
		t.Fatal("the manual task should run while automatic tasks are disabled")
	}
	awaitCodeArts(t, ch, "hw-1/manual")
}
