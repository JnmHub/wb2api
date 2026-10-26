package pool

import (
	"testing"
	"time"

	"wb2api/internal/store/accounts"
)

// A model-scoped 429 window must survive a restart: it is written to the record
// and re-armed by the loader instead of being forgotten with the process.
func TestModelCooldownSurvivesRestart(t *testing.T) {
	now := time.Now()
	rec := &accounts.Record{UID: "u1", Realm: "intl", AccessToken: "tok", ExpiresAt: now.Add(2 * time.Hour).Unix()}
	acc := New([]*accounts.Record{rec}).Get("u1")
	if acc == nil {
		t.Fatal("account missing")
	}
	persisted, isNew := acc.NoteModelError("gpt-5.5", now.Add(30*time.Minute), 0, "429", "", now)
	if !isNew || persisted == nil {
		t.Fatalf("first throttle returned (%v, %v), want a record", persisted, isNew)
	}
	if _, ok := persisted.ModelCool["gpt-5.5"]; !ok {
		t.Fatalf("record carries no cooldown: %v", persisted.ModelCool)
	}
	if _, again := acc.NoteModelError("gpt-5.5", now.Add(20*time.Minute), 0, "429", "", now); again {
		t.Fatal("a shorter window inside the same cooldown rewrote the record")
	}

	restarted := New([]*accounts.Record{persisted}).Get("u1")
	if !restarted.HasModelCooldown(now) {
		t.Fatal("model cooldown lost across restart")
	}
	if restarted.HasModelCooldown(now.Add(31 * time.Minute)) {
		t.Fatal("expired cooldown still reported after the reset instant")
	}
}
