package identity

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"wb2api/internal/realm"
)

var kindRe = map[string]*regexp.Regexp{
	"hex32":       regexp.MustCompile(`^[0-9a-f]{32}$`),
	"uuid":        regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`),
	"hex16":       regexp.MustCompile(`^[0-9a-f]{16}$`),
	"ms13":        regexp.MustCompile(`^\d{13}$`),
	"traceparent": regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`),
	"b3_3":        regexp.MustCompile(`^[0-9a-f]{32}-[0-9a-f]{16}-1$`),
	"b3_4":        regexp.MustCompile(`^[0-9a-f]{32}-[0-9a-f]{16}-1-[0-9a-f]{16}$`),
}

type goldHeader struct {
	Name  string  `json:"name"`
	Value *string `json:"value"`
	Kind  string  `json:"kind"`
}

type goldCase struct {
	Product    string       `json:"product"`
	Realm      string       `json:"realm"`
	Enterprise string       `json:"enterprise"`
	Session    *string      `json:"session"`
	Model      *string      `json:"model"`
	Conv       *string      `json:"conv"`
	Turn       *string      `json:"turn"`
	Token      string       `json:"token"`
	Headers    []goldHeader `json:"headers"`
}

func sv(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// TestChatHeadersGolden compares all 192 product/realm/enterprise/session/
// model/conversation/turn combinations with the legacy output: same headers in
// the same order, byte-identical stable values, and the same shape for the
// random ones.
func TestChatHeadersGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/headers.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []goldCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 192 {
		t.Fatalf("expected 192 vectors, got %d", len(cases))
	}
	t.Setenv("WB_IDENTITY_OS", "Windows")
	t.Setenv("WB_IDENTITY_ARCH", "x64")
	t.Setenv("WB_IDENTITY_NODE_VERSION", "v22.21.1")
	t.Setenv("WB_VSCODE_IDE_VERSION", "1.119.0")
	for i, c := range cases {
		// The account record falls back to the token's enterprise_id claim.
		ent := c.Enterprise
		if ent == "" {
			ent = realm.JWTEnterpriseID(c.Token)
		}
		got := ChatHeaders(ChatParams{Params: Params{
			Product: c.Product, Realm: c.Realm, UID: "uid-x", Token: c.Token,
			ConversationID: sv(c.Conv), EnterpriseID: ent, TenantID: ent,
			Model: sv(c.Model), SessionKey: sv(c.Session), TurnRequestID: sv(c.Turn),
		}})
		label := func() string {
			return strings.Join([]string{c.Product, c.Realm, "ent=" + c.Enterprise, "sess=" + sv(c.Session), "model=" + sv(c.Model), "conv=" + sv(c.Conv), "turn=" + sv(c.Turn)}, " ")
		}
		if len(got) != len(c.Headers) {
			names := make([]string, len(got))
			for j, p := range got {
				names[j] = p.Name
			}
			want := make([]string, len(c.Headers))
			for j, p := range c.Headers {
				want[j] = p.Name
			}
			t.Errorf("#%d %s: %d headers, want %d\n got  %v\n want %v", i, label(), len(got), len(c.Headers), names, want)
			continue
		}
		for j, w := range c.Headers {
			g := got[j]
			if g.Name != w.Name {
				t.Errorf("#%d %s: header %d is %q, want %q", i, label(), j, g.Name, w.Name)
				break
			}
			if w.Value != nil {
				if g.Value != *w.Value {
					t.Errorf("#%d %s: %s = %q, want %q", i, label(), w.Name, g.Value, *w.Value)
				}
			} else if re := kindRe[w.Kind]; re == nil || !re.MatchString(g.Value) {
				t.Errorf("#%d %s: %s = %q does not look like %s", i, label(), w.Name, g.Value, w.Kind)
			}
		}
	}
}

func TestConversationIDGolden(t *testing.T) {
	raw, _ := os.ReadFile("testdata/conv_ids.json")
	var vs []struct {
		UID string `json:"uid"`
		Key string `json:"key"`
		Out string `json:"out"`
	}
	if err := json.Unmarshal(raw, &vs); err != nil || len(vs) == 0 {
		t.Fatal(err)
	}
	for _, v := range vs {
		if got := ConversationIDFor(v.UID, v.Key); got != v.Out {
			t.Errorf("ConversationIDFor(%q,%q)=%q want %q", v.UID, v.Key, got, v.Out)
		}
	}
}

// Relationships between random ids that the upstream could cross-check.
func TestIDRelationships(t *testing.T) {
	h := BuildHeaders(Params{Product: ProductCLI, Realm: "intl", UID: "u", Token: "t", TurnRequestID: "turn-9"})
	get := func(n string) string { v, _ := h.Get(n); return v }
	if get("X-Conversation-Request-ID") != "turn-9" || get("X-Root-Request-ID") != "turn-9" {
		t.Error("CLI turn id must be reused for conversation-request and root-request")
	}
	if get("X-Request-ID") != get("X-Conversation-Message-ID") {
		t.Error("CLI: X-Request-ID must equal X-Conversation-Message-ID")
	}
	if get("X-Trace-ID") != get("X-B3-TraceId") || !strings.HasSuffix(get("b3"), "-"+get("X-B3-ParentSpanId")) {
		t.Error("trace headers must agree with each other")
	}
	v := BuildHeaders(Params{Product: ProductVSCode, Realm: "intl", UID: "u", Token: "t"})
	vg := func(n string) string { x, _ := v.Get(n); return x }
	if vg("X-Request-ID") != vg("X-Conversation-Request-ID") {
		t.Error("VSCode: X-Request-ID is the turn request id")
	}
	if _, ok := v.Get("X-Root-Request-ID"); ok {
		t.Error("VSCode main conversation must not send X-Root-Request-ID")
	}
}

func TestUUID7Shape(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	for i := 0; i < 50; i++ {
		if s := NewConversationID(); !re.MatchString(s) {
			t.Fatalf("not a UUIDv7: %s", s)
		}
	}
	if NewConversationID() == NewConversationID() || NewMessageID() == NewMessageID() {
		t.Fatal("ids must differ")
	}
	re4 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if s := NewRequestTraceID(); !re4.MatchString(s) {
		t.Fatalf("not a UUIDv4: %s", s)
	}
}

func TestHeadersSetKeepsPosition(t *testing.T) {
	h := Headers{{"A", "1"}, {"B", "2"}}
	h.Set("A", "9")
	h.Set("C", "3")
	if h[0] != (Pair{"A", "9"}) || h[2] != (Pair{"C", "3"}) || len(h) != 3 {
		t.Fatalf("%v", h)
	}
}
