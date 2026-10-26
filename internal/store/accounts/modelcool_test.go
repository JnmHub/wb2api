package accounts

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// modelCool round-trips through the on-disk format so a restart reloads the
// same cooldown windows.
func TestModelCoolRoundTrip(t *testing.T) {
	rec := &Record{UID: "u1", Realm: "intl", AccessToken: "tok", ModelCool: map[string]float64{"gpt-5.5": 1760000000.5}}
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
	if got := back.ModelCool["gpt-5.5"]; got != 1760000000.5 {
		t.Fatalf("modelCool = %v, want 1760000000.5 (raw %s)", got, raw)
	}
}
