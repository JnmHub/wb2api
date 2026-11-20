package settings

import (
	"testing"

	"wb2api/internal/realm"
)

func TestRaccoonKeysStayBoundAndHiddenFromOldLoader(t *testing.T) {
	store := Open(t.TempDir())
	if err := store.Save(Doc{"api_keys": []any{
		map[string]any{"id": "old-rc", "key": "fixture-old-raccoon", "realm": realm.Raccoon, "enabled": true, "deleted_at": ""},
		map[string]any{"id": "wb", "key": "fixture-workbuddy", "realm": realm.Intl, "enabled": true, "deleted_at": ""},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.ProtectRaccoonKeys(); err != nil {
		t.Fatal(err)
	}
	rc, _ := findKey(rawKeys(store.View()), "old-rc")
	wb, _ := findKey(rawKeys(store.View()), "wb")
	if rc["key"] != "" || wb["key"] != "fixture-workbuddy" {
		t.Fatal("legacy key field must hide only the Raccoon secret")
	}
	keys := store.APIKeys(false)
	if len(keys) != 2 || !keys[0].Enabled || keys[0].Realm != realm.Raccoon || keys[0].Key != "fixture-old-raccoon" {
		t.Fatalf("current gateway did not retain the Raccoon binding: %d keys", len(keys))
	}
	rcRealm := realm.Raccoon
	otherSecret := "fixture-new-raccoon"
	newKey, err := store.CreateKey(KeyInput{Realm: &rcRealm, Key: &otherSecret})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateKey(KeyInput{Key: &otherSecret}); err == nil {
		t.Fatal("accepted duplicate of a protected Raccoon secret")
	}
	unbound := ""
	if err := store.UpdateKey(newKey.ID, KeyInput{Realm: &unbound}); err != nil {
		t.Fatal(err)
	}
	row, _ := findKey(rawKeys(store.View()), newKey.ID)
	if row["key"] != otherSecret || row[raccoonKeyField] != nil {
		t.Fatal("explicit realm change did not move the secret back to the normal field")
	}
	if err := store.DeleteKey("old-rc"); err != nil {
		t.Fatal(err)
	}
	rc, _ = findKey(rawKeys(store.View()), "old-rc")
	if storedKeySecret(rc) != "" {
		t.Fatal("deleted key still has a secret")
	}
}
