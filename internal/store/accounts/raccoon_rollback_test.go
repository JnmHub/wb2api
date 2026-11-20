package accounts

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"wb2api/internal/realm"
)

func TestProtectRaccoonFilesKeepsPortableRecordsAndHidesFromLegacyLoader(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	legacyPath := filepath.Join(dir, "rc-sample.json")
	legacy := []byte(`{"uid":"rc-sample","realm":" Raccoon ","accessToken":"fixture-at","refreshToken":"fixture-rt","enabled":true,"weight":4}`)
	if err := os.WriteFile(legacyPath, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	recs, bad := Load(dir, now)
	if len(recs) != 1 || len(bad) != 0 {
		t.Fatalf("loading pre-release file: %d records, %v", len(recs), bad)
	}
	if err := ProtectRaccoonFiles(dir, recs, now); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	data, err := decodeObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	// The v2.11.3 file loader calls FromMap directly and skips empty UIDs.
	old := FromMap(data, legacyPath, now)
	if old.UID != "" || old.AccessToken != "" || old.RefreshToken != "" {
		t.Fatal("an older file loader could read the protected account")
	}
	current, bad := Load(dir, now)
	if len(current) != 1 || len(bad) != 0 || current[0].Realm != realm.Raccoon || current[0].AccessToken != "fixture-at" || current[0].RefreshToken != "fixture-rt" || current[0].Weight != 4 {
		t.Fatalf("current file loader did not preserve the account: %v", bad)
	}
	backup, err := current[0].Marshal()
	if err != nil {
		t.Fatal(err)
	}
	doc, err := decodeObject(backup)
	if err != nil || doc["uid"] != "rc-sample" || doc[raccoonRecordField] != nil {
		t.Fatal("portable exports should use the ordinary account schema")
	}
	refreshed := *current[0]
	refreshed.AccessToken = "new-fixture-at"
	if _, err := SaveReconciled(dir, &refreshed, now); err != nil {
		t.Fatal(err)
	}
	stale := *current[0]
	stale.ObservedRaccoonCredential = current[0]
	stale.Note = "result from an old credential"
	if live, err := SaveReconciled(dir, &stale, now); !errors.Is(err, ErrConflict) || live == nil || live.Note != "" || live.AccessToken != refreshed.AccessToken {
		t.Fatalf("stale provider result reached the file store: %v", err)
	}
	if _, err := Save(dir, &Record{UID: "rc-sample", Realm: realm.Intl}); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-channel save replaced the protected file: %v", err)
	}
}
