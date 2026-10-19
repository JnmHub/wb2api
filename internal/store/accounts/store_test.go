package accounts

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type expected struct {
	File               string         `json:"file"`
	UID                string         `json:"uid"`
	Nickname           string         `json:"nickname"`
	Domain             string         `json:"domain"`
	Realm              string         `json:"realm"`
	Platform           string         `json:"platform"`
	Product            string         `json:"product"`
	EnterpriseID       string         `json:"enterprise_id"`
	AccessToken        string         `json:"access_token"`
	RefreshToken       string         `json:"refresh_token"`
	ExpiresAt          int64          `json:"expires_at"`
	Source             string         `json:"source"`
	ProxySlot          string         `json:"proxy_slot"`
	ProxyLegacy        string         `json:"proxy_legacy"`
	Enabled            bool           `json:"enabled"`
	Weight             int            `json:"weight"`
	Note               string         `json:"note"`
	DisabledModels     []string       `json:"disabled_models"`
	RestrictedUntil    float64        `json:"restricted_until"`
	Credits            map[string]any `json:"credits"`
	LastCheckin        any            `json:"last_checkin"`
	LastDailyChat      any            `json:"last_daily_chat"`
	LastActivityReport any            `json:"last_activity_report"`
	ModelCaps          map[string]struct {
		At    float64 `json:"at"`
		Reset float64 `json:"reset"`
	} `json:"model_caps"`
}

func loadExpected(t *testing.T) []expected {
	t.Helper()
	raw, err := os.ReadFile("testdata/py_expected.json")
	if err != nil {
		t.Fatal(err)
	}
	var out []expected
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertRecord(t *testing.T, label string, r *Record, e expected) {
	t.Helper()
	chk := func(name string, got, want any) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s %s: got %#v want %#v", label, name, got, want)
		}
	}
	chk("uid", r.UID, e.UID)
	chk("nickname", r.Nickname, e.Nickname)
	chk("domain", r.Domain, e.Domain)
	chk("realm", r.Realm, e.Realm)
	chk("platform", r.Platform, e.Platform)
	chk("product", r.Product, e.Product)
	chk("enterprise_id", r.EnterpriseID, e.EnterpriseID)
	chk("access_token", r.AccessToken, e.AccessToken)
	chk("refresh_token", r.RefreshToken, e.RefreshToken)
	chk("expires_at", r.ExpiresAt, e.ExpiresAt)
	chk("source", r.Source, e.Source)
	chk("proxy_slot", r.ProxySlot, e.ProxySlot)
	chk("proxy_legacy", r.ProxyLegacy, e.ProxyLegacy)
	chk("enabled", r.IsEnabled(), e.Enabled)
	chk("weight", r.Weight, e.Weight)
	chk("note", r.Note, e.Note)
	if len(r.DisabledModels) != 0 || len(e.DisabledModels) != 0 {
		chk("disabled_models", r.DisabledModels, e.DisabledModels)
	}
	chk("restricted_until", r.RestrictedUntil, e.RestrictedUntil)
	if len(r.Credits) != 0 || len(e.Credits) != 0 {
		// compare through JSON so json.Number vs float64 does not matter
		a, _ := json.Marshal(r.Credits)
		b, _ := json.Marshal(e.Credits)
		chk("credits", string(a), string(b))
	}
	chk("last_checkin", r.LastCheckin, e.LastCheckin)
	chk("last_daily_chat", r.LastDailyChat, e.LastDailyChat)
	chk("last_activity_report", r.LastActivityReport, e.LastActivityReport)
	if len(r.ModelCaps) != len(e.ModelCaps) {
		t.Errorf("%s model_caps: got %v want %v", label, r.ModelCaps, e.ModelCaps)
	}
	for k, v := range e.ModelCaps {
		if got, ok := r.ModelCaps[k]; !ok || got.At != v.At || got.Reset != v.Reset {
			t.Errorf("%s model_caps[%s]: got %v want %v", label, k, got, v)
		}
	}
}

// TestLoadPythonWritten loads a directory written by the legacy Python code
// and checks every field against the attributes Python derived.
func TestLoadPythonWritten(t *testing.T) {
	exp := loadExpected(t)
	recs, errs := Load("testdata/py_written", time.Unix(1_800_000_000, 0))
	if len(errs) != 1 || errs[0].File != "broken.json" {
		t.Fatalf("expected exactly broken.json to fail, got %v", errs)
	}
	if len(recs) != len(exp) {
		t.Fatalf("loaded %d accounts, want %d", len(recs), len(exp))
	}
	byFile := map[string]*Record{}
	for _, r := range recs {
		byFile[filepath.Base(r.Path)] = r
	}
	for _, e := range exp {
		r := byFile[e.File]
		if r == nil {
			t.Fatalf("missing %s", e.File)
		}
		assertRecord(t, e.File, r, e)
	}
}

// TestRoundTrip: load -> save into a temp dir -> load again must be stable, and
// unknown keys must survive.
func TestRoundTrip(t *testing.T) {
	exp := loadExpected(t)
	recs, _ := Load("testdata/py_written", time.Unix(1_800_000_000, 0))
	for _, r := range recs {
		if r.UID == "intl-uid-1" {
			r.Extra["futureField"] = map[string]any{"a": json.Number("1")}
		}
	}
	dir := t.TempDir()
	for _, r := range recs {
		if _, err := Save(dir, r); err != nil {
			t.Fatal(err)
		}
	}
	back, errs := Load(dir, time.Unix(1_800_000_000, 0))
	if len(errs) != 0 {
		t.Fatalf("errors on reload: %v", errs)
	}
	byFile := map[string]*Record{}
	for _, r := range back {
		byFile[filepath.Base(r.Path)] = r
	}
	for _, e := range exp {
		r := byFile[e.File]
		if r == nil {
			t.Fatalf("round trip lost %s", e.File)
		}
		assertRecord(t, "roundtrip "+e.File, r, e)
	}
	if byFile["intl-uid-1.json"].Extra["futureField"] == nil {
		t.Error("unknown field was dropped")
	}
	// no temp files left behind
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
}

func TestMarshalFormat(t *testing.T) {
	r := FromMap(map[string]any{"uid": "u1", "accessToken": "t", "note": "<b>&</b> 中文"}, "", time.Unix(1_800_000_000, 0))
	out, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"note": "<b>&</b> 中文"`)) {
		t.Errorf("html/unicode escaped or not indented like python:\n%s", out)
	}
	if !bytes.HasPrefix(out, []byte("{\n  \"uid\": \"u1\",")) {
		t.Errorf("unexpected prefix:\n%s", out)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"lastError", "cooldownUntil"} {
		if _, ok := m[k]; ok {
			t.Errorf("runtime-only field %s must not be persisted", k)
		}
	}
}

func TestSaveUnsafeUIDFallsBackToRandomName(t *testing.T) {
	r := FromMap(map[string]any{"uid": "___", "accessToken": "t"}, "", time.Now())
	dir := t.TempDir()
	p, err := Save(dir, r)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(p) != dir || filepath.Ext(p) != ".json" {
		t.Errorf("bad path %s", p)
	}
}

// TestWriteForPythonCrossCheck writes the fixture accounts through the Go
// writer into $WB_CROSSCHECK_OUT so tools/crosscheck/py_load.py can verify the
// legacy Python loader accepts them. Skipped unless the variable is set.
func TestWriteForPythonCrossCheck(t *testing.T) {
	out := os.Getenv("WB_CROSSCHECK_OUT")
	if out == "" {
		t.Skip("WB_CROSSCHECK_OUT not set")
	}
	recs, _ := Load("testdata/py_written", time.Unix(1_800_000_000, 0))
	for _, r := range recs {
		if _, err := Save(out, r); err != nil {
			t.Fatal(err)
		}
	}
}
