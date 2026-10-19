package atrest

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wb2api/internal/realm"
)

func testJWT(sub string, exp int64, iss string) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]any{"alg": "none", "typ": "JWT"}) + "." +
		enc(map[string]any{"sub": sub, "exp": exp, "iss": iss}) + ".sig"
}

func testKeyring(t *testing.T) (*Keyring, []byte, []byte) {
	t.Helper()
	secretBytes := make([]byte, 32)
	for i := range secretBytes {
		secretBytes[i] = byte(i + 1)
	}
	secret := base64.StdEncoding.EncodeToString(secretBytes)
	key := DeriveKey(secret)
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(100 + i)
	}
	wrapped, err := Seal(key, master, "file")
	if err != nil {
		t.Fatal(err)
	}
	blob := map[string]any{
		"keyId": KeyID(master),
		"slots": []any{map[string]any{
			"type": "static-v1", "protectorKeyId": KeyID(key), "wrapped": wrapped,
		}},
	}
	path := filepath.Join(t.TempDir(), "keyblob")
	b, _ := json.Marshal(blob)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return &Keyring{BlobPath: path}, key, []byte(secret)
}

func TestSealAndOpenRoundTripForEveryFraming(t *testing.T) {
	key := DeriveKey("some-secret-text")
	for _, framing := range []string{"file", "field", "record", "stream"} {
		env, err := Seal(key, []byte("plain-"+framing), framing)
		if err != nil {
			t.Fatal(err)
		}
		got, err := OpenEnvelope(key, env, framing)
		if err != nil {
			t.Fatalf("%s: %v", framing, err)
		}
		if string(got) != "plain-"+framing {
			t.Fatalf("%s: %q", framing, got)
		}
		// A different framing must fail authentication.
		if _, err := OpenEnvelope(key, env, "record"); framing != "record" && err == nil {
			t.Fatalf("%s opened under the wrong framing", framing)
		}
	}
}

func TestEnvelopeRejectsWrongKeyAndTampering(t *testing.T) {
	key := DeriveKey("right")
	env, _ := Seal(key, []byte("payload"), "field")
	if _, err := OpenEnvelope(DeriveKey("wrong"), env, "field"); err == nil {
		t.Fatal("a wrong key must fail")
	}
	m := map[string]any{"$wbEncrypted": 1}
	raw, _ := base64.StdEncoding.DecodeString(env["envelope"].(string))
	var e map[string]any
	_ = json.Unmarshal(raw, &e)
	ct, _ := base64.StdEncoding.DecodeString(e["ciphertext"].(string))
	ct[0] ^= 0xff
	e["ciphertext"] = base64.StdEncoding.EncodeToString(ct)
	tampered, _ := json.Marshal(e)
	m["envelope"] = base64.StdEncoding.EncodeToString(tampered)
	if _, err := OpenEnvelope(key, m, "field"); err == nil {
		t.Fatal("tampered ciphertext must fail")
	}
}

func TestParseKeyText(t *testing.T) {
	raw := make([]byte, 32)
	key := DeriveKey(base64.StdEncoding.EncodeToString(raw))
	hexKey := hex.EncodeToString(key)
	got, err := ParseKeyText("0x" + strings.ToUpper(hexKey))
	if err != nil || KeyID(got) != KeyID(key) {
		t.Fatalf("hex: %v", err)
	}
	got, err = ParseKeyText(base64.StdEncoding.EncodeToString(raw))
	if err != nil || KeyID(got) != KeyID(key) {
		t.Fatalf("base64: %v", err)
	}
	if _, err := ParseKeyText("nonsense"); err == nil {
		t.Fatal("junk must be rejected")
	}
}

func TestKeyringUnwrapsAndAdopts(t *testing.T) {
	kr, key, secret := testKeyring(t)
	master, err := kr.Unwrap(key)
	if err != nil || len(master) != 32 {
		t.Fatalf("%v len=%d", err, len(master))
	}
	bad := make([]byte, 32)
	if _, err := kr.Unwrap(bad); err == nil {
		t.Fatal("a wrong protector key must fail")
	}
	if got, err := kr.Adopt(string(secret)); err != nil || KeyID(got) != KeyID(key) {
		t.Fatalf("adopt: %v", err)
	}
	if kr.Cached() == nil {
		t.Fatal("the adopted key must be cached")
	}
	if st := kr.Status(); st["key_cached"] != true || st["keyblob_found"] != true {
		t.Fatalf("status: %v", st)
	}
	kr.Forget()
	if kr.Cached() != nil {
		t.Fatal("forget must drop the key")
	}
}

func TestScanAndCredential(t *testing.T) {
	kr, key, _ := testKeyring(t)
	dir := t.TempDir()
	token := testJWT("user-123", time.Now().Add(48*time.Hour).Unix(), "https://www.codebuddy.cn")
	tokenEnv, _ := EnvelopeString(key, []byte(token), "field")
	nickEnv, _ := EnvelopeString(key, []byte("测试昵称"), "field")
	doc := map[string]any{
		"auth": map[string]any{"accessToken": map[string]any{"$wbEncrypted": 1, "envelope": tokenEnv},
			"refreshToken": "", "expiresAt": time.Now().Add(48 * time.Hour).Unix(), "domain": "copilot.tencent.com"},
		"account": map[string]any{"uid": "user-123", "nickname": map[string]any{"$wbEncrypted": 1, "envelope": nickEnv}},
	}
	b, _ := json.Marshal(doc)
	path := filepath.Join(dir, "workbuddy-desktop.info")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	// Point the scan at the temp directory.
	old := os.Getenv("LOCALAPPDATA")
	_ = os.Setenv("LOCALAPPDATA", dir)
	defer func() { _ = os.Setenv("LOCALAPPDATA", old) }()
	// candidates() looks for <LOCALAPPDATA>/CodeBuddyExtension/Data/Public/auth
	authDir := filepath.Join(dir, "CodeBuddyExtension", "Data", "Public", "auth")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, filepath.Join(authDir, "workbuddy-desktop.info")); err != nil {
		t.Fatal(err)
	}

	items := kr.Scan()
	if len(items) != 1 || !items[0].NeedsKey {
		t.Fatalf("without a key the item must ask for one: %+v", items)
	}
	if _, err := kr.Adopt(base64.StdEncoding.EncodeToString(key)); err == nil {
		t.Fatal("adopting a different secret must fail")
	}
	// Adopt the real secret via the key itself (64-hex), which the keyblob validates.
	if _, err := kr.Adopt(hex.EncodeToString(key)); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	items = kr.Scan()
	if len(items) != 1 || !items[0].Valid || items[0].UID != "user-123" || items[0].Nickname != "测试昵称" {
		t.Fatalf("item: %+v", items[0])
	}
	if items[0].Realm != realm.CN {
		t.Fatalf("realm: %s", items[0].Realm)
	}
	row, err := kr.Credential(items[0].Path, "cn", kr.Cached())
	if err != nil {
		t.Fatal(err)
	}
	if row["uid"] != "user-123" || row["accessToken"] != token || row["realm"] != realm.CN || row["source"] != "desktop-app" {
		t.Fatalf("row: %v", row)
	}
}
