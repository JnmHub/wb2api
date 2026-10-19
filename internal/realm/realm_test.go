package realm

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func load(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytesReader(raw))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		t.Fatal(err)
	}
}

func TestJWTAndRealmGolden(t *testing.T) {
	var vs []struct {
		Token     string            `json:"token"`
		Domain    *string           `json:"domain"`
		Exp       int64             `json:"exp"`
		UID       string            `json:"uid"`
		Iss       string            `json:"iss"`
		Ent       string            `json:"ent"`
		Evidence  *string           `json:"evidence"`
		Realm     string            `json:"realm"`
		DomainFor map[string]string `json:"domain_for"`
	}
	load(t, "jwt_realm.json", &vs)
	if len(vs) == 0 {
		t.Fatal("no vectors")
	}
	for i, v := range vs {
		dom := ""
		if v.Domain != nil {
			dom = *v.Domain
		}
		if got := JWTExp(v.Token); got != v.Exp {
			t.Errorf("#%d JWTExp(%q)=%d want %d", i, v.Token, got, v.Exp)
		}
		if got := JWTUID(v.Token); got != v.UID {
			t.Errorf("#%d JWTUID=%q want %q", i, got, v.UID)
		}
		if got := JWTIssuer(v.Token); got != v.Iss {
			t.Errorf("#%d JWTIssuer=%q want %q", i, got, v.Iss)
		}
		if got := JWTEnterpriseID(v.Token); got != v.Ent {
			t.Errorf("#%d JWTEnterpriseID=%q want %q", i, got, v.Ent)
		}
		wantEv := ""
		if v.Evidence != nil {
			wantEv = *v.Evidence
		}
		if got := Evidence(v.Token, dom); got != wantEv {
			t.Errorf("#%d Evidence(%q,%q)=%q want %q", i, v.Token, dom, got, wantEv)
		}
		if got := Detect(v.Token, dom); got != v.Realm {
			t.Errorf("#%d Detect=%q want %q", i, got, v.Realm)
		}
		for r, want := range v.DomainFor {
			if got := DomainFor(r, dom); got != want {
				t.Errorf("#%d DomainFor(%q,%q)=%q want %q", i, r, dom, got, want)
			}
		}
	}
}

func TestNormalizeEpochGolden(t *testing.T) {
	var vs []struct {
		In  any   `json:"in"`
		Out int64 `json:"out"`
	}
	load(t, "epoch.json", &vs)
	for _, v := range vs {
		if got := NormalizeEpoch(v.In); got != v.Out {
			t.Errorf("NormalizeEpoch(%#v)=%d want %d", v.In, got, v.Out)
		}
	}
}

func TestDisabledModelsGolden(t *testing.T) {
	var vs []struct {
		In  any      `json:"in"`
		Out []string `json:"out"`
	}
	load(t, "disabled_models.json", &vs)
	for _, v := range vs {
		got := NormaliseDisabledModels(v.In)
		if len(got) == 0 && len(v.Out) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, v.Out) {
			t.Errorf("NormaliseDisabledModels(%#v)=%v want %v", v.In, got, v.Out)
		}
	}
}

func TestFreeCapGolden(t *testing.T) {
	var raw []map[string]any
	load(t, "free_cap.json", &raw)
	for _, m := range raw {
		got := IsFreeCapModel(m["realm"].(string), m["model"].(string))
		if got != m["out"].(bool) {
			t.Errorf("IsFreeCapModel(%v,%v)=%v want %v", m["realm"], m["model"], got, m["out"])
		}
	}
}

func TestCNChatBase(t *testing.T) {
	t.Setenv("WB_CN_CHAT_UPSTREAM", "")
	if b, ig := CNChatBase(); b != DefaultCNChatBase || ig != "" {
		t.Fatalf("default: %q %q", b, ig)
	}
	t.Setenv("WB_CN_CHAT_UPSTREAM", "https://www.workbuddy.cn/")
	if b, ig := CNChatBase(); b != "https://www.workbuddy.cn" || ig != "" {
		t.Fatalf("override: %q %q", b, ig)
	}
	t.Setenv("WB_CN_CHAT_UPSTREAM", "ftp://x")
	if b, ig := CNChatBase(); b != DefaultCNChatBase || ig != "ftp://x" {
		t.Fatalf("invalid: %q %q", b, ig)
	}
}

func TestDetectModelRealmGolden(t *testing.T) {
	var vs []struct {
		Model, Current, Out string
	}
	var raw []map[string]string
	load(t, "model_realm.json", &raw)
	_ = vs
	for _, m := range raw {
		if got := DetectModelRealm(m["model"], m["current"]); got != m["out"] {
			t.Errorf("DetectModelRealm(%q,%q)=%q want %q", m["model"], m["current"], got, m["out"])
		}
	}
	if len(raw) == 0 {
		t.Fatal("no vectors")
	}
}
