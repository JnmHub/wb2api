package codearts

import (
	"testing"
	"time"
	"wb2api/internal/realm"

	"wb2api/internal/store/accounts"
)

func TestCredentialOfNestedAndFlatLayouts(t *testing.T) {
	nested := &accounts.Record{
		UID:         "hw-1",
		Realm:       "codearts",
		AccessToken: "STS",
		Extra: map[string]any{ExtraKey: map[string]any{
			"access_key_id":     "AK",
			"secret_access_key": "SK",
			"expiration":        "2026-11-18T00:00:00Z",
		}},
	}
	got := CredentialOf(nested)
	if got.AccessKeyID != "AK" || got.SecretAccessKey != "SK" || got.SecurityToken != "STS" {
		t.Fatalf("nested credential = %+v", got)
	}

	flat := &accounts.Record{
		UID: "hw-2", Realm: "codearts",
		Extra: map[string]any{"accessKeyId": "AK2", "secretAccessKey": "SK2", "securityToken": "STS2"},
	}
	got = CredentialOf(flat)
	if got.AccessKeyID != "AK2" || got.SecretAccessKey != "SK2" || got.SecurityToken != "STS2" {
		t.Fatalf("flat credential = %+v", got)
	}
	if c := CredentialOf(nil); c.AccessKeyID != "" {
		t.Fatalf("nil record produced %+v", c)
	}
}

// Refresh must never mutate the live account while it writes the new pair: the
// record is copied by value, and its Extra map would otherwise be shared.
func TestApplyTokenDoesNotAliasTheOriginalExtra(t *testing.T) {
	rec := &accounts.Record{
		UID: "hw-1", Realm: "codearts", AccessToken: "STS-OLD", RefreshToken: "R-OLD",
		Extra: map[string]any{ExtraKey: map[string]any{
			"access_key_id": "AK-OLD", "secret_access_key": "SK-OLD", "dpop_private_key": map[string]any{"kty": "EC"},
		}},
	}
	exp := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	next := ApplyToken(rec, &TokenResponse{
		RefreshToken: "R-NEW",
		Credentials: Credential{
			AccessKeyID: "AK-NEW", SecretAccessKey: "SK-NEW", SecurityToken: "STS-NEW",
			Expiration: exp.Format(time.RFC3339),
		},
	})
	if rec.AccessToken != "STS-OLD" || rec.RefreshToken != "R-OLD" {
		t.Fatal("the original record was mutated")
	}
	old := rec.Extra[ExtraKey].(map[string]any)
	if old["access_key_id"] != "AK-OLD" {
		t.Fatal("the original credential map was mutated")
	}
	if next.AccessToken != "STS-NEW" || next.RefreshToken != "R-NEW" || next.ExpiresAt != exp.Unix() {
		t.Fatalf("refreshed record = %+v", next)
	}
	got := CredentialOf(next)
	if got.AccessKeyID != "AK-NEW" || got.SecretAccessKey != "SK-NEW" {
		t.Fatalf("refreshed credential = %+v", got)
	}
	// The DPoP key must survive the update: losing it makes the account
	// unrefreshable forever.
	if DPoPKeyOf(next)["kty"] != "EC" {
		t.Fatal("dpop key was dropped by the refresh")
	}
}

func TestDPoPKeyOfReadsBothShapes(t *testing.T) {
	rec := &accounts.Record{Extra: map[string]any{ExtraKey: map[string]any{
		"dpop_private_key": map[string]any{"kty": "EC", "d": "abc"},
	}}}
	if k := DPoPKeyOf(rec); k["d"] != "abc" {
		t.Fatalf("dpop key = %v", k)
	}
	rec2 := &accounts.Record{Extra: map[string]any{ExtraKey: map[string]any{
		"dpop_private_key": map[string]string{"kty": "EC", "d": "xyz"},
	}}}
	if k := DPoPKeyOf(rec2); k["d"] != "xyz" {
		t.Fatalf("dpop key = %v", k)
	}
	if k := DPoPKeyOf(&accounts.Record{}); k != nil {
		t.Fatalf("missing key = %v", k)
	}
}

func TestModelsOfAndBenefitModels(t *testing.T) {
	rec := &accounts.Record{Extra: map[string]any{ExtraKey: map[string]any{
		"models":         []any{"GLM-5.2", "glm-5.2", "Qwen3-VL-235B", "GLM-5.2"},
		"benefit_models": []any{"Qwen3-VL-235B"},
	}}}
	got := ModelsOf(rec)
	// Exact duplicates collapse; a case variant is kept because the upstream
	// treats GLM-5.2 and glm-5.2 as two acceptable spellings of one model.
	if len(got) != 3 || got[0] != "GLM-5.2" || got[1] != "glm-5.2" || got[2] != "Qwen3-VL-235B" {
		t.Fatalf("models = %v", got)
	}
	if !IsBenefitModel(rec, "qwen3-vl-235b") {
		t.Fatal("benefit membership must be case-insensitive")
	}
	if IsBenefitModel(rec, "GLM-5.2") {
		t.Fatal("GLM-5.2 is not on the benefit list")
	}
	if got := ModelsOf(&accounts.Record{Extra: map[string]any{ExtraKey: map[string]any{"models": "a, b;c"}}}); len(got) != 3 {
		t.Fatalf("comma list = %v", got)
	}
	if got := ModelsOf(nil); got != nil {
		t.Fatalf("nil record = %v", got)
	}
}

// The catalogue carries both spellings; only one of them is callable.
func TestWireNameTranslatesDisplayNames(t *testing.T) {
	rec := &accounts.Record{Realm: realm.CodeArts, Extra: map[string]any{
		"codearts": map[string]any{
			"models":      []any{"deepseek-v4.1-flash", "GLM-5.2"},
			"model_names": map[string]any{"deepseek-v4.1-flash": "DeepSeek-V4.1-Flash"},
		},
	}}
	cases := map[string]string{
		"deepseek-v4.1-flash": "deepseek-v4.1-flash", // already the wire name
		"DeepSeek-V4.1-Flash": "deepseek-v4.1-flash", // display name → wire id
		"deepseek-V4.1-FLASH": "deepseek-v4.1-flash", // match is case-insensitive
		"GLM-5.2":             "GLM-5.2",             // no mapping needed
		"unknown-model":       "unknown-model",       // pass through untouched
	}
	for in, want := range cases {
		if got := WireName(rec, in); got != want {
			t.Errorf("WireName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := WireName(nil, "x"); got != "x" {
		t.Fatalf("nil record must pass through: %q", got)
	}
}
