package pgaccounts

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
)

func assertLegacyPGCannotLoad(t *testing.T, raw []byte, key string) {
	t.Helper()
	var old map[string]any
	if err := json.Unmarshal(raw, &old); err != nil {
		t.Fatal(err)
	}
	// The old SQL decoder rejects a nonempty top-level UID differing from
	// the primary key before constructing a record.
	if uid, _ := old["uid"].(string); uid == "" || uid == key || old["accessToken"] != nil || old["refreshToken"] != nil {
		t.Fatal("stored row is visible to the older account decoder")
	}
}

func TestRaccoonPostgresRecordEncoding(t *testing.T) {
	const uid = "rc-postgres"
	record := &accounts.Record{UID: uid, Realm: realm.Raccoon, AccessToken: "synthetic-at", RefreshToken: "synthetic-rt"}
	raw, err := record.MarshalPostgres()
	if err != nil {
		t.Fatal(err)
	}
	assertLegacyPGCannotLoad(t, raw, uid)
	current, err := decode(uid, raw, time.Now())
	if err != nil || current.UID != uid || current.AccessToken != record.AccessToken || current.RefreshToken != record.RefreshToken {
		t.Fatalf("current decoder did not retain the Raccoon record: %v", err)
	}
	if _, err := decode("another-primary-key", raw, time.Now()); err == nil {
		t.Fatal("a protected row bypassed SQL primary key validation")
	}
}

func TestPostgresProtectsRaccoonRowsAndReconcilesTheirChanges(t *testing.T) {
	ctx, db := syncTestDB(t)
	const uid = "rc-migrate"
	if _, err := db.Exec(ctx, `insert into wb_accounts(uid,data) values($1,'{"realm":" Raccoon ","accessToken":"synthetic-at","refreshToken":"synthetic-rt"}')`, uid); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := LoadSnapshot(ctx, db, ""); err != nil {
		t.Fatal(err)
	}
	for _, otherRealm := range []string{realm.Intl, realm.CodeArts, realm.Raccoon} {
		if err := Save(ctx, db, &accounts.Record{UID: uid, Realm: otherRealm}); !errors.Is(err, ErrConflict) {
			t.Fatalf("fresh insert for %s overwrote a protected row: %v", otherRealm, err)
		}
	}
	first, err := LoadOne(ctx, db, uid)
	if err != nil {
		t.Fatal(err)
	}
	metadata := *first
	metadata.Note = "independent metadata change"
	other := *first
	other.AccessToken = "newer-token"
	if err := Save(ctx, db, &other); err != nil {
		t.Fatal(err)
	}
	stale := metadata
	stale.ObservedRaccoonCredential = first
	if err := Save(ctx, db, &stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale result from a different credential was accepted: %v", err)
	}
	if err := Save(ctx, db, &metadata); err != nil {
		t.Fatal(err)
	}
	next, err := LoadOne(ctx, db, uid)
	if err != nil || next.Note != metadata.Note || next.AccessToken != other.AccessToken {
		t.Fatalf("stored row did not merge both independent changes: %v", err)
	}
	var raw []byte
	if err := db.QueryRow(ctx, `select data from wb_accounts where uid=$1`, uid).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	assertLegacyPGCannotLoad(t, raw, uid)
}

func TestPostgresRaccoonAccountLocksCoordinateReplicas(t *testing.T) {
	ctx, db := syncTestDB(t)
	release, err := LockRaccoonAccount(ctx, db, "maintenance", "rc-fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	blocked, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if unlock, err := LockRaccoonAccount(blocked, db, "maintenance", "rc-fixture"); err == nil {
		unlock()
		t.Fatal("another replica entered the same account maintenance scope")
	} else if blocked.Err() == nil {
		t.Fatalf("lock failed before waiting for the original holder: %v", err)
	}
	// A maintenance operation can renew the credential under a different lock.
	allowRefresh, err := LockRaccoonAccount(ctx, db, "refresh", "rc-fixture")
	if err != nil {
		t.Fatal(err)
	}
	allowRefresh()
	release()
	allowNext, err := LockRaccoonAccount(ctx, db, "maintenance", "rc-fixture")
	if err != nil {
		t.Fatal(err)
	}
	allowNext()
}
