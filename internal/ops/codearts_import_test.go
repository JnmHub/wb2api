package ops

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream/codearts"
)

// codeartsImport is the shape codexarts and the official tooling produce.
const codeartsImport = `{
  "user_id": "hw-900",
  "user_name": "华为用户",
  "cloud_dragon_token": "STS-TOKEN",
  "access_key_id": "AK1",
  "secret_access_key": "SK1",
  "expiration": "2026-12-01T00:00:00Z",
  "refresh_token": "refresh-1",
  "client_id": "codearts-agent",
  "code_verifier": "verifier-1",
  "dpop_private_key": {"kty":"EC","crv":"P-256","x":"x","y":"y","d":"d"}
}`

func newImportClient(t *testing.T) (*Client, *pool.Pool, *[]*accounts.Record) {
	t.Helper()
	p := pool.New(nil)
	saved := []*accounts.Record{}
	c := &Client{Pool: p, Now: func() time.Time { return time.Now() },
		Persist: func(r *accounts.Record) error { saved = append(saved, r); return nil }}
	return c, p, &saved
}

// A CodeArts credential is not a JWT: it must take its own import path instead
// of being rejected by the WorkBuddy token checks.
func TestImportCodeArtsCredential(t *testing.T) {
	rows, err := ParseDocument([]byte(codeartsImport))
	if err != nil {
		t.Fatalf("ParseDocument: %v", err)
	}
	c, p, saved := newImportClient(t)
	rep := c.Import(rows, "", false, false)
	if len(rep.Added) != 1 || rep.Added[0] != "hw-900" {
		t.Fatalf("report = %+v (invalid: %v)", rep, rep.Invalid)
	}
	acc := p.Get("hw-900")
	if acc == nil {
		t.Fatal("account missing from the pool")
	}
	rec := acc.Record()
	if rec.Realm != realm.CodeArts || rec.AccessToken != "STS-TOKEN" || rec.RefreshToken != "refresh-1" {
		t.Fatalf("record = %+v", rec)
	}
	if rec.ExpiresAt != time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("expiresAt = %d", rec.ExpiresAt)
	}
	cred := codearts.CredentialOf(rec)
	if cred.AccessKeyID != "AK1" || cred.SecretAccessKey != "SK1" || cred.SecurityToken != "STS-TOKEN" {
		t.Fatalf("credential = %+v", cred)
	}
	if codearts.DPoPKeyOf(rec)["d"] != "d" {
		t.Fatal("dpop key lost: the credential could never be refreshed")
	}
	if codearts.CodeVerifierOf(rec) != "verifier-1" {
		t.Fatalf("code_verifier = %q", codearts.CodeVerifierOf(rec))
	}
	if len(*saved) != 1 {
		t.Fatalf("persisted %d records", len(*saved))
	}
}

// A list of mixed channels imports each row on its own path.
func TestImportMixedChannels(t *testing.T) {
	// Not a JWT and no CodeArts fields: it must stay invalid for WorkBuddy.
	doc := `[{"accessToken":"opaque-token","uid":"wb-1","realm":"intl"}]`
	rows, err := ParseDocument([]byte(doc))
	if err != nil {
		t.Fatalf("ParseDocument: %v", err)
	}
	c, _, _ := newImportClient(t)
	rep := c.Import(rows, "", false, true) // dry run: the JWT is not real
	if len(rep.Invalid) != 1 {
		t.Fatalf("a bogus JWT stays invalid: %+v", rep)
	}

	rows2, err := ParseDocument([]byte(codeartsImport))
	if err != nil {
		t.Fatalf("ParseDocument codearts: %v", err)
	}
	if norm, err := NormaliseRow(rows2[0], "intl"); err != nil || norm["realm"] != realm.CodeArts {
		t.Fatalf("forced realm must not move a CodeArts credential: %v %v", norm["realm"], err)
	}
}

func TestImportCodeArtsRejectsIncompleteRow(t *testing.T) {
	rows := []Row{{"cloud_dragon_token": "STS", "user_id": "hw-1"}}
	c, _, _ := newImportClient(t)
	rep := c.Import(rows, "", false, false)
	if len(rep.Invalid) != 1 || !strings.Contains(rep.Invalid[0]["reason"].(string), "access_key_id") {
		t.Fatalf("report = %+v", rep)
	}
}

func TestImportAgent2APICodeArtsExport(t *testing.T) {
	key, err := codearts.NewDPoPKey()
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	row := Row{
		"provider": "codearts", "id": "agent-local-id", "user_id": "hw-agent",
		"domain_id": "hw-domain", "accessToken": "AK1", "access_key_id": "AK1",
		"secret_access_key": "SK1", "security_token": "STS1", "expiresAt": expiry.UnixMilli(),
		"refresh_token": "rt", "name": "供迁移的账号",
		"oauth_context": Row{
			"pkce_pair":     Row{"code_verifier": "verifier"},
			"dpop_key_pair": Row{"private_key_jwk": key},
		},
	}
	raw, _ := json.Marshal(map[string]any{"version": 1, "accounts": []Row{row}})
	rows, err := ParseDocument(raw)
	if err != nil {
		t.Fatal(err)
	}
	c, p, saved := newImportClient(t)
	if rep := c.Import(rows, realm.CodeArts, false, true); len(rep.Added) != 1 || len(*saved) != 0 {
		t.Fatalf("dry-run: %+v", rep)
	}
	if rep := c.Import(rows, realm.CodeArts, false, false); len(rep.Added) != 1 {
		t.Fatalf("import: %+v", rep)
	}
	rec := p.Get("hw-agent").Record()
	if rec.AccessToken != "STS1" || rec.ExpiresAt != expiry.Unix() ||
		codearts.ClientIDOf(rec) != "vscode-codebot" || codearts.CodeVerifierOf(rec) != "verifier" ||
		codearts.DPoPKeyOf(rec)["d"] == "" || rec.CodeArtsFields()["domain_id"] != "hw-domain" {
		t.Fatalf("incorrect credential projection for %s", rec.UID)
	}
	delete(rows[0], "security_token")
	if rep := c.Import(rows, realm.CodeArts, true, true); len(rep.Invalid) != 1 {
		t.Fatalf("generic accessToken carries the AK and cannot replace STS: %+v", rep)
	}
}

// A secret-free export must not leak the AK/SK chain.
func TestExportWithoutSecretsDropsCodeArtsCredential(t *testing.T) {
	p := pool.New([]*accounts.Record{{
		UID: "hw-1", Realm: realm.CodeArts, AccessToken: "STS", RefreshToken: "r",
		Extra: map[string]any{"codearts": map[string]any{
			"access_key_id": "AK1", "secret_access_key": "SK1", "dpop_private_key": map[string]any{"d": "d"},
		}},
	}})
	c := &Client{Pool: p, Now: func() time.Time { return time.Now() }}
	doc, err := c.Export(realm.CodeArts, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(doc)
	for _, secret := range []string{"AK1", "SK1", `"d"`} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("secret-free export leaked %s: %s", secret, raw)
		}
	}
	// With secrets it is included (that is what the export is for).
	doc, _ = c.Export(realm.CodeArts, nil, true)
	raw, _ = json.Marshal(doc)
	if !strings.Contains(string(raw), "AK1") {
		t.Fatalf("full export lost the credential: %s", raw)
	}
}
