package settings

import (
	"strings"
	"testing"
)

func ptr[T any](v T) *T { return &v }

func TestKeyLifecycle(t *testing.T) {
	s := Open(t.TempDir())
	k, err := s.CreateKey(KeyInput{Name: ptr("cursor"), Realm: ptr("cn"), Models: ptr([]string{"glm-*"}), ConcurrencyLimit: ptr(3)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.Key, "wb-") || len(k.Key) < 20 || k.Realm != "cn" || k.ConcurrencyLimit != 3 || !k.AllowsModel("glm-5.3") || k.AllowsModel("gpt-5.5") {
		t.Fatalf("%+v", k)
	}
	if got, ok := s.RevealKey(k.ID); !ok || got != k.Key {
		t.Fatal("reveal")
	}
	// a partial update keeps everything it does not mention
	if err := s.UpdateKey(k.ID, KeyInput{Name: ptr("renamed")}); err != nil {
		t.Fatal(err)
	}
	got := s.APIKeys(false)[0]
	if got.Name != "renamed" || got.Realm != "cn" || got.ConcurrencyLimit != 3 || got.Key != k.Key {
		t.Fatalf("partial update lost fields: %+v", got)
	}
	if err := s.UpdateKey(k.ID, KeyInput{Enabled: ptr(false)}); err != nil || s.APIKeys(false)[0].Enabled {
		t.Fatalf("disable: %v", err)
	}
	// validation
	if _, err := s.CreateKey(KeyInput{Key: ptr("abc")}); err == nil {
		t.Error("short keys are refused")
	}
	if _, err := s.CreateKey(KeyInput{Realm: ptr("mars")}); err == nil {
		t.Error("unknown realm")
	}
	if _, err := s.CreateKey(KeyInput{Key: ptr(k.Key)}); err == nil {
		t.Error("duplicate secret")
	}
	// soft delete: the row stays for usage attribution, the secret is gone
	if err := s.DeleteKey(k.ID); err != nil {
		t.Fatal(err)
	}
	if len(s.APIKeys(false)) != 0 {
		t.Error("deleted keys do not authenticate")
	}
	all := s.APIKeys(true)
	if len(all) != 1 || all[0].ID != k.ID || all[0].Key != "" || all[0].DeletedAt == "" {
		t.Fatalf("history row: %+v", all)
	}
	if _, ok := s.RevealKey(k.ID); ok {
		t.Error("a deleted secret is gone")
	}
	if err := s.UpdateKey(k.ID, KeyInput{Name: ptr("x")}); err == nil {
		t.Error("a deleted key cannot be revived")
	}
}

func TestProxySlotIDsNeverRecycle(t *testing.T) {
	s := Open(t.TempDir())
	out, err := s.SetProxySlots([]ProxySlot{{URL: "http://a:1", Enabled: true}, {URL: "socks5://b:2", Enabled: true}})
	if err != nil || out[0].ID != "slot-1" || out[1].ID != "slot-2" {
		t.Fatalf("%v %v", out, err)
	}
	// drop slot-2, add a new one: it must not reuse slot-2
	out, _ = s.SetProxySlots([]ProxySlot{out[0], {URL: "http://c:3", Enabled: true}})
	if out[1].ID != "slot-3" {
		t.Fatalf("freed ids must not be recycled: %v", out)
	}
	if _, err := s.SetProxySlots([]ProxySlot{{URL: "ftp://x"}}); err == nil {
		t.Error("bad scheme")
	}
	if got := s.ProxySlots(); len(got) != 2 || got[1].URL != "http://c:3" {
		t.Fatalf("%v", got)
	}
}

func TestLimitsAndSwitches(t *testing.T) {
	s := Open(t.TempDir())
	if v := s.Limit(LimitExpiringWindow, "intl"); v != 7 {
		t.Fatalf("the expiring window ships enabled: %d", v)
	}
	if _, err := s.SetLimit(LimitDailyToken, "global", ptr(1000)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetLimit(LimitDailyToken, "cn", ptr(50)); err != nil {
		t.Fatal(err)
	}
	if s.Limit(LimitDailyToken, "intl") != 1000 || s.Limit(LimitDailyToken, "cn") != 50 {
		t.Fatal("realm override beats the global value")
	}
	if _, err := s.SetLimit(LimitDailyToken, "cn", nil); err != nil || s.Limit(LimitDailyToken, "cn") != 1000 {
		t.Fatal("clearing an override inherits again")
	}
	if _, err := s.SetLimit("nope", "global", ptr(1)); err == nil {
		t.Error("unknown limit")
	}
	if err := s.SetSwitch("egress_backoff_enabled", true); err != nil || !s.EgressBackoff() {
		t.Fatal("switch")
	}
	if err := s.SetSwitch("made_up", true); err == nil {
		t.Error("unknown switch")
	}
	if got, err := s.SetAutoSwitchProducts([]string{"cli", "cli", "vscode"}); err != nil || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := s.SetAutoSwitchProducts([]string{"mars"}); err == nil {
		t.Error("unknown identity")
	}
}

func TestUpstreamAndPromptValidation(t *testing.T) {
	s := Open(t.TempDir())
	cfg, err := s.SetUpstream(map[string]any{"header_timeout_seconds": 30, "device_token": " abc "})
	if err != nil || cfg.HeaderTimeout.Seconds() != 30 || cfg.DeviceToken != "abc" {
		t.Fatalf("%+v %v", cfg, err)
	}
	if _, err := s.SetUpstream(map[string]any{"idle_timeout_seconds": 0}); err == nil {
		t.Error("zero timeout")
	}
	if _, err := s.SetUpstream(map[string]any{"bogus": 1}); err == nil {
		t.Error("unknown key")
	}
	if p, err := s.SetPrompt(map[string]any{"mode": "APPEND"}); err != nil || p.Mode != "append" {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := s.SetPrompt(map[string]any{"mode": "weird"}); err == nil {
		t.Error("bad mode")
	}
	if err := s.SetCreditsRefreshHours(6); err != nil || s.CreditsRefreshHours() != 6 {
		t.Fatal("credits ttl")
	}
}
