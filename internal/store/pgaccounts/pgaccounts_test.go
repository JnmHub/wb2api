package pgaccounts

import (
	"testing"
	"time"
)

func TestDecodeRejectsMismatchedUID(t *testing.T) {
	if _, err := decode("database-key", []byte(`{"uid":"different-uid"}`), time.Now()); err == nil {
		t.Fatal("embedded UID mismatch could escape SQL-keyed deletion")
	}
}

func TestDecodeKeepsUidAndNumbers(t *testing.T) {
	rec, err := decode("u1", []byte(`{"uid":"u1","nickname":"n","weight":3}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rec.UID != "u1" || rec.Nickname != "n" || rec.Weight != 3 || rec.Path != "" {
		t.Fatalf("record = %+v", rec)
	}
}
