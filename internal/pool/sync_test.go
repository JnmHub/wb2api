package pool

import (
	"testing"
	"time"

	"wb2api/internal/store/accounts"
)

func TestSyncRecordsPreservesLocalCreditUntilAuthoritativeRefresh(t *testing.T) {
	sharedRecord := func(remain, updatedAt float64, note string) *accounts.Record {
		t.Helper()
		rec := &accounts.Record{
			UID: "u", Realm: "cn", AccessToken: "token", Enabled: true, Weight: 1, Note: note,
			Credits: map[string]any{"remain": remain, "used": 100 - remain, "size": 100.0, "updated_at": updatedAt},
		}
		raw, err := rec.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		rec.SharedBaseline = raw
		return rec
	}
	p := New([]*accounts.Record{sharedRecord(100, 1, "")})
	a := p.Get("u")
	a.ConsumeCredit(25)
	for _, rec := range []*accounts.Record{sharedRecord(100, 1, ""), sharedRecord(100, 1, "remote edit")} {
		p.SyncRecords([]*accounts.Record{rec})
		if remain, ok := a.CreditRemain(); !ok || remain != 75 {
			t.Fatalf("unchanged shared credits reset local estimate: remain=%v ok=%v", remain, ok)
		}
		if a.Record().Note != rec.Note || p.Get("u") != a {
			t.Fatal("unrelated management edit or account identity lost")
		}
		if remain, _ := toNumber(rec.Credits["remain"]); remain != 100 {
			t.Fatal("sync mutated the incoming shared snapshot")
		}
	}
	// A real refresh replaces the estimate, even if it reports the same
	// balance as before: updated_at proves this is a newly fetched snapshot.
	p.SyncRecords([]*accounts.Record{sharedRecord(100, 2, "remote edit")})
	if remain, _ := a.CreditRemain(); remain != 100 {
		t.Fatalf("authoritative refresh ignored: remain=%v", remain)
	}
	a.ConsumeCredit(10)
	p.SyncRecords([]*accounts.Record{sharedRecord(100, 2, "remote edit")})
	if remain, _ := a.CreditRemain(); remain != 90 {
		t.Fatalf("poll after refresh reset local estimate: remain=%v", remain)
	}
	p.SyncRecords([]*accounts.Record{sharedRecord(60, 3, "remote edit")})
	if remain, _ := a.CreditRemain(); remain != 60 {
		t.Fatalf("changed authoritative balance ignored: remain=%v", remain)
	}
}

func TestSameSharedCreditsIgnoresDocumentOrder(t *testing.T) {
	before := []byte(`{"credits":{"remain":100,"updated_at":1},"note":"before"}`)
	next := []byte(`{"note":"after", "credits": {"updated_at":1,"remain":100}}`)
	if !sameSharedCredits(before, next) {
		t.Fatal("document formatting or unrelated fields changed credits equality")
	}
	if sameSharedCredits(nil, next) || sameSharedCredits(before, []byte(`{"credits":null}`)) {
		t.Fatal("missing baseline or deleted credits treated as unchanged")
	}
}

func TestSyncRecordsPreservesRuntimeAndRemovesDeleted(t *testing.T) {
	rec := &accounts.Record{UID: "u", Realm: "cn", AccessToken: "token", Enabled: true, Weight: 1}
	p := New([]*accounts.Record{rec})
	a := p.Get("u")
	a.inflight.Store(2)
	until := time.Now().Add(time.Hour).UnixNano()
	a.cooldownTo.Store(until)
	a.tokenBad.Store(true)
	a.restrictedTo.Store(until)
	next := *rec
	next.Weight = 5
	next.Enabled = false
	p.SyncRecords([]*accounts.Record{&next})
	if p.Get("u") != a || a.Inflight() != 2 || a.cooldownTo.Load() != until || a.restrictedTo.Load() != until || !a.tokenBad.Load() || a.Weight() != 5 || a.enabled.Load() {
		t.Fatal("runtime state lost or management change ignored")
	}
	p.SyncRecords(nil)
	if p.Get("u") != nil || a.enabled.Load() || a.Inflight() != 2 {
		t.Fatal("deleted account retained or in-flight state lost")
	}
}
