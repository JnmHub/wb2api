package openai

import (
	"strings"
	"testing"

	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
)

// An upstream answer that echoes the rejected credential must not reach the
// client or the ledger with the secret intact.
func TestRedactSecretsMasksCredentials(t *testing.T) {
	rec := &accounts.Record{
		Realm: realm.CodeArts, AccessToken: "STS-abcdef123456", RefreshToken: "RT-abcdef123456",
		Extra: map[string]any{"codearts": map[string]any{
			"access_key_id": "AKIAEXAMPLE12345", "secret_access_key": "SKsecret/with+chars",
		}},
	}
	body := `{"error_msg":"bad signature for AKIAEXAMPLE12345 with token STS-abcdef123456 and SKsecret/with+chars"}`
	out := string(redactBody([]byte(body), rec))
	for _, secret := range []string{"AKIAEXAMPLE12345", "STS-abcdef123456", "SKsecret/with+chars"} {
		if strings.Contains(out, secret) {
			t.Fatalf("secret %q survived: %s", secret, out)
		}
	}
	if !strings.Contains(out, "bad signature for") {
		t.Fatalf("redaction destroyed the message: %s", out)
	}

	// URL-escaped echoes are masked too.
	esc := `{"error_msg":"token SKsecret%2Fwith%2Bchars rejected"}`
	if out := string(redactBody([]byte(esc), rec)); strings.Contains(out, "SKsecret") {
		t.Fatalf("escaped form survived: %s", out)
	}
}

func TestWithUpstreamCode(t *testing.T) {
	if got := withUpstreamCode("upstream 400: invalid request parameters", "TM.00001050"); got != "upstream 400: invalid request parameters (TM.00001050)" {
		t.Fatalf("got %q", got)
	}
	if got := withUpstreamCode("already mentions TM.00001050", "TM.00001050"); strings.Count(got, "TM.00001050") != 1 {
		t.Fatalf("duplicated code: %q", got)
	}
	if got := withUpstreamCode("plain", ""); got != "plain" {
		t.Fatalf("got %q", got)
	}
}
