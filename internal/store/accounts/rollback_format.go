package accounts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"wb2api/internal/realm"
)

const raccoonRecordField = "raccoon_record_v1"

// MarshalStored hides the Raccoon record from binaries that do not know its
// realm. File readers skip a document without a top-level UID. Keep Marshal
// as the portable account format for exports and three-way merges.
func (r *Record) MarshalStored() ([]byte, error) { return r.marshalStored(false) }

// MarshalPostgres uses a deliberately different top-level UID so the older
// PostgreSQL reader, which fills missing UIDs from the SQL key, skips the row.
func (r *Record) MarshalPostgres() ([]byte, error) { return r.marshalStored(true) }

func (r *Record) marshalStored(postgres bool) ([]byte, error) {
	raw, err := r.Marshal()
	if err != nil || !realm.IsRaccoon(r.Realm) {
		return raw, err
	}
	doc := map[string]any{raccoonRecordField: json.RawMessage(raw)}
	if postgres {
		doc["uid"] = r.UID + "#raccoon-storage-v1"
	}
	return marshalNoEscape(doc)
}

// UnwrapStoredRecord validates the storage envelope before the reader applies
// its normal account decoding and SQL key checks.
func UnwrapStoredRecord(data map[string]any) (map[string]any, error) {
	value, protected := data[raccoonRecordField]
	if !protected {
		return data, nil
	}
	record, ok := value.(map[string]any)
	uid, _ := record["uid"].(string)
	if !ok || uid == "" || record["realm"] != realm.Raccoon {
		return nil, errors.New("invalid stored Raccoon account")
	}
	if v, exists := data["uid"]; exists && v != uid+"#raccoon-storage-v1" {
		return nil, errors.New("invalid stored Raccoon account UID")
	}
	if _, nested := record[raccoonRecordField]; nested {
		return nil, errors.New("nested Raccoon account envelope")
	}
	return record, nil
}

func decodeStoredRecord(data map[string]any, path string, now time.Time) (*Record, error) {
	data, err := UnwrapStoredRecord(data)
	if err != nil {
		return nil, err
	}
	return FromMap(data, path, now), nil
}

// ProtectRaccoonFiles migrates files written by pre-release builds before the
// accounts are published into the pool. Existing envelopes need no rewrite.
func ProtectRaccoonFiles(dir string, records []*Record, now time.Time) error {
	for i, rec := range records {
		if !realm.IsRaccoon(rec.Realm) {
			continue
		}
		raw, err := os.ReadFile(rec.Path)
		if err != nil {
			return fmt.Errorf("read Raccoon account for migration: %w", err)
		}
		doc, err := decodeObject(raw)
		if err != nil {
			return err
		}
		if _, migrated := doc[raccoonRecordField]; migrated {
			continue
		}
		latest, err := SaveReconciled(dir, rec, now)
		if err != nil {
			return fmt.Errorf("protect Raccoon account: %w", err)
		}
		records[i] = latest
	}
	return nil
}
