package accounts_test

import (
	"path/filepath"
	"testing"
	"time"

	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream/codearts"
)

// A CodeArts account file must survive the load/save round trip with its AK/SK
// chain intact: the refresher and the signer both read it back, and a lost
// dpop_private_key makes the account unrefreshable forever.
func TestCodeArtsRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	rec := accounts.FromMap(map[string]any{
		"uid": "hw-1", "realm": "codearts",
		"accessToken": "STS-1", "refreshToken": "refresh-1",
		"expiresAt": float64(now.Add(2 * time.Hour).Unix()),
		"codearts": map[string]any{
			"access_key_id": "AK-1", "secret_access_key": "SK-1",
			"security_token": "STS-1", "client_id": "codearts-agent",
			"dpop_private_key": map[string]any{"kty": "EC", "crv": "P-256", "x": "x", "y": "y", "d": "d"},
			"models":           []any{"GLM-5.2"},
		},
	}, "", now)
	if rec.Realm != realm.CodeArts {
		t.Fatalf("realm = %q", rec.Realm)
	}
	// The channel has no WorkBuddy domain: defaulting it would write
	// www.workbuddy.ai into a Huawei account file.
	if rec.Domain != "" {
		t.Fatalf("domain = %q, want empty", rec.Domain)
	}
	if _, err := accounts.Save(dir, rec); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, errs := accounts.Load(dir, now)
	if len(errs) != 0 || len(loaded) != 1 {
		t.Fatalf("Load: %d records, %d errors: %v", len(loaded), len(errs), errs)
	}
	got := loaded[0]
	if got.Realm != realm.CodeArts || got.AccessToken != "STS-1" || got.RefreshToken != "refresh-1" {
		t.Fatalf("round trip lost the token chain: %+v", got)
	}
	cred := codearts.CredentialOf(got)
	if cred.AccessKeyID != "AK-1" || cred.SecretAccessKey != "SK-1" {
		t.Fatalf("credential lost: %+v", cred)
	}
	if codearts.DPoPKeyOf(got)["d"] != "d" {
		t.Fatal("dpop key lost")
	}
	if models := codearts.ModelsOf(got); len(models) != 1 || models[0] != "GLM-5.2" {
		t.Fatalf("models lost: %v", models)
	}
	if filepath.Base(got.Path) != "hw-1.json" {
		t.Fatalf("path = %s", got.Path)
	}
}
