package ratelimit

import (
	"encoding/json"
	"os"
	"testing"
)

type vec struct {
	Bodies []struct {
		Body         string   `json:"body"`
		Reset        *float64 `json:"reset"`
		Scope        string   `json:"scope"`
		Soft         bool     `json:"soft"`
		Cap          bool     `json:"cap"`
		AccountLevel bool     `json:"account_level"`
		Restriction  bool     `json:"restriction"`
	} `json:"bodies"`
}

func TestAgainstLegacy(t *testing.T) {
	raw, err := os.ReadFile("testdata/ratelimit.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vec
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Bodies {
		reset, ok := ParseReset(c.Body)
		if ok != (c.Reset != nil) || (ok && float64(reset.Unix()) != *c.Reset) {
			t.Errorf("ParseReset(%q) = %v,%v want %v", c.Body, reset.Unix(), ok, c.Reset)
		}
		if got := ScopePhrase(c.Body); got != c.Scope {
			t.Errorf("ScopePhrase(%q) = %q want %q", c.Body, got, c.Scope)
		}
		if got := IsSoftShape(c.Body); got != c.Soft {
			t.Errorf("IsSoftShape(%q) = %v want %v", c.Body, got, c.Soft)
		}
		if got := IsCap(c.Body); got != c.Cap {
			t.Errorf("IsCap(%q) = %v want %v", c.Body, got, c.Cap)
		}
		if got := IsAccountLevel(c.Body, c.Reset != nil); got != c.AccountLevel {
			t.Errorf("IsAccountLevel(%q) = %v want %v", c.Body, got, c.AccountLevel)
		}
		if got := IsRestriction(c.Body); got != c.Restriction {
			t.Errorf("IsRestriction(%q) = %v want %v", c.Body, got, c.Restriction)
		}
	}
}
