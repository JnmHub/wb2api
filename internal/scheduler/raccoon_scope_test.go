package scheduler

import (
	"testing"

	"time"

	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
)

// TestWorkBuddyScopeExcludesOtherChannels locks the one behavioural change the
// raccoon channel made to existing code: the daily-chore sweep must stay exactly
// the WorkBuddy accounts.
func TestWorkBuddyScopeExcludesOtherChannels(t *testing.T) {
	p := pool.New([]*accounts.Record{
		{UID: "intl-1", Realm: realm.Intl},
		{UID: "cn-1", Realm: realm.CN},
		{UID: "hw-1", Realm: realm.CodeArts},
		{UID: "rc-1", Realm: realm.Raccoon},
	})
	var got []string
	for _, acc := range p.All() {
		if workBuddyScope(acc) {
			got = append(got, acc.UID)
		}
	}
	if len(got) != 2 || got[0] != "intl-1" || got[1] != "cn-1" {
		t.Fatalf("workbuddy scope = %v, want [intl-1 cn-1]", got)
	}
	if workBuddyScope(nil) {
		t.Fatal("nil account must not be in scope")
	}
}

// TestCheckedInToday keeps a restart from re-running the day's raccoon task.
func TestCheckedInToday(t *testing.T) {
	now := time.Date(2026, 11, 20, 13, 0, 0, 0, time.UTC) // 21:00 Beijing
	rec := &accounts.Record{UID: "rc-1", Realm: realm.Raccoon,
		LastCheckin: map[string]any{"at": "2026-11-20 00:31:12", "ok": true, "message": "今日积分 +300"}}
	if !checkedInToday(rec, now) {
		t.Fatal("today's successful stamp must block an automatic re-run")
	}
	if checkedInToday(&accounts.Record{LastCheckin: map[string]any{"at": "2026-11-19 00:31:12", "ok": true}}, now) {
		t.Fatal("yesterday's stamp must not block")
	}
	if checkedInToday(&accounts.Record{LastCheckin: map[string]any{"at": "2026-11-20 00:31:12", "ok": false}}, now) {
		t.Fatal("a failed stamp must not block a retry")
	}
	if checkedInToday(nil, now) || checkedInToday(&accounts.Record{}, now) {
		t.Fatal("a missing stamp must not block")
	}
}
