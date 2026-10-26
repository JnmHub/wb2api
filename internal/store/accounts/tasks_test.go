package accounts

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// Legacy records have no tasksEnabled key and must keep taking chores.
func TestTasksOnDefaultsToTrue(t *testing.T) {
	rec := FromMap(map[string]any{"uid": "u1", "enabled": true}, "u1.json", time.Now())
	if !rec.TasksOn() {
		t.Fatal("a record without tasksEnabled must still take chores")
	}
	for _, v := range []any{true, "true", json.Number("1"), float64(1)} {
		if !(&Record{TasksEnabled: v}).TasksOn() {
			t.Fatalf("tasksEnabled=%#v must be on", v)
		}
	}
	for _, v := range []any{false, "false", "0", "", json.Number("0"), float64(0)} {
		if (&Record{TasksEnabled: v}).TasksOn() {
			t.Fatalf("tasksEnabled=%#v must be off", v)
		}
	}
	if (&Record{TasksEnabled: "maybe"}).TasksOn() != true {
		t.Fatal("an unexpected value must not silently disable chores")
	}
}

// The switch and the cached tier survive a rewrite of the account file.
func TestTasksAndTierRoundTrip(t *testing.T) {
	rec := &Record{UID: "u1", Realm: "intl", AccessToken: "tok", TasksEnabled: false, Tier: "plus"}
	raw, err := rec.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&data); err != nil {
		t.Fatal(err)
	}
	back := FromMap(data, filepath.Join("accounts", "u1.json"), time.Now())
	if back.TasksOn() {
		t.Fatalf("tasksEnabled lost in rewrite (raw %s)", raw)
	}
	if back.Tier != "plus" {
		t.Fatalf("tier = %q, want plus (raw %s)", back.Tier, raw)
	}
	if _, ok := back.Extra["tasksEnabled"]; ok {
		t.Fatal("tasksEnabled must be a known key, not an opaque extra")
	}
	// A record that never set the switch keeps the file byte-compatible with
	// the legacy writer: the key is present and true, never missing.
	plain := &Record{UID: "u2", Realm: "intl", AccessToken: "tok"}
	rawPlain, err := plain.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(rawPlain, []byte(`"tasksEnabled": true`)) {
		t.Fatalf("plain record must carry tasksEnabled true (raw %s)", rawPlain)
	}
}
