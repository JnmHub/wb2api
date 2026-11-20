package ops

import (
	"encoding/json"
	"strings"
	"testing"

	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream/raccoon"
)

func TestRaccoonExportRedactsCredentialsFromAllRecordLayouts(t *testing.T) {
	legacy := &accounts.Record{UID: "rc-legacy", Realm: realm.Raccoon, Extra: map[string]any{
		"extra": map[string]any{raccoon.ExtraKey: map[string]any{"access_token": "nested-at", "refresh_token": "nested-rt"}},
		"token": "flat-token", "access_token": "flat-at", "refresh_token": "flat-rt",
	}}
	rotated := raccoon.ApplyToken(legacy, raccoon.Credential{Token: "rotated-at", RefreshToken: "rotated-rt", OrgCode: "visible-org"})
	c := &Client{Pool: pool.New([]*accounts.Record{rotated})}
	doc, err := c.Export(realm.Raccoon, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"flat-token", "flat-at", "flat-rt", "nested-at", "nested-rt", "rotated-at", "rotated-rt"} {
		if strings.Contains(string(raw), marker) {
			t.Errorf("secret-free export contains synthetic credential %s", marker)
		}
	}
	if !strings.Contains(string(raw), "visible-org") {
		t.Fatal("secret-free export dropped organization metadata")
	}
}

func TestRaccoonAccountRoundTripAndCrossRealmGuard(t *testing.T) {
	const token = "fixture.eyJzdWIiOiJmaXh0dXJlIn0.signature"
	source := &accounts.Record{UID: "rc-fixture", Realm: realm.Raccoon, AccessToken: token, RefreshToken: "fixture-rt", Enabled: false,
		Note: "preserve", Extra: map[string]any{raccoon.ExtraKey: map[string]any{"officeIdentity": "fixture-org"}}}
	c := &Client{Pool: pool.New([]*accounts.Record{source})}
	doc, err := c.Export(realm.Raccoon, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ParseDocument(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, forced := range []string{"", realm.Raccoon} {
		fresh := &Client{Pool: pool.New(nil)}
		report := fresh.Import(rows, forced, true, false)
		if len(report.Added) != 1 {
			t.Fatalf("round-trip report: %#v", report)
		}
		a := fresh.Pool.Get(source.UID)
		if a == nil || a.Realm != realm.Raccoon || a.Enabled() || a.Record().RaccoonOrgCode() != "fixture-org" || a.Record().Note != "preserve" || a.Record().RefreshToken != "fixture-rt" {
			t.Fatal("round-trip did not retain channel, settings, credentials and organization")
		}
	}
	foreign := &accounts.Record{UID: source.UID, Realm: realm.Intl, AccessToken: "foreign"}
	existing := &Client{Pool: pool.New([]*accounts.Record{foreign})}
	for _, dry := range []bool{true, false} {
		report := existing.Import(rows, realm.Raccoon, true, dry)
		if len(report.Invalid) != 1 || len(report.Updated) != 0 || existing.Pool.Get(source.UID).Record() != foreign {
			t.Fatalf("cross-realm overwrite accepted, dry=%v report=%#v", dry, report)
		}
	}
}
