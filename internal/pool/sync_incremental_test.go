package pool

import (
	"testing"
	"time"

	"wb2api/internal/store/accounts"
)

func incrementalRecord(t *testing.T, uid, realm string, balance, stamp float64, note string) *accounts.Record {
	t.Helper()
	r := &accounts.Record{UID: uid, Realm: realm, AccessToken: "token", Enabled: true, Weight: 1, Note: note,
		Credits: map[string]any{"remain": balance, "used": 100 - balance, "updated_at": stamp}}
	raw, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	r.SharedBaseline = raw
	return r
}

func TestApplyChangesPreservesPointersRuntimeAndCredits(t *testing.T) {
	p := New([]*accounts.Record{incrementalRecord(t, "u", "cn", 100, 1, ""), incrementalRecord(t, "other", "cn", 100, 1, "")})
	a, other := p.Get("u"), p.Get("other")
	a.ConsumeCredit(25)
	a.Acquire()
	a.NoteResult(false)
	a.MarkTokenBad(true)
	until := time.Now().Add(time.Hour).UnixNano()
	a.cooldownTo.Store(until)
	a.restrictedTo.Store(until)
	oldSnapshot := p.snap.Load()
	p.prefDirty.Store(false)
	p.ApplyChanges(nil, nil)
	p.ApplyChanges([]*accounts.Record{incrementalRecord(t, "u", "cn", 100, 1, "")}, nil)
	if p.prefDirty.Load() || p.snap.Load() != oldSnapshot {
		t.Fatal("unchanged polling invalidated preferences or membership")
	}
	next := incrementalRecord(t, "u", "cn", 100, 1, "management edit")
	p.ApplyChanges([]*accounts.Record{next}, nil)
	if !p.prefDirty.Load() || p.snap.Load() != oldSnapshot || p.Get("u") != a || p.Get("other") != other {
		t.Fatal("record-only change rebuilt membership or lost identities")
	}
	if n, _ := a.CreditRemain(); n != 75 || a.Record().Note != next.Note || a.Inflight() != 1 || a.Snapshot().Requests != 1 || a.cooldownTo.Load() != until || a.restrictedTo.Load() != until || !a.tokenBad.Load() {
		t.Fatal("incremental sync reset live state or missed management edit")
	}
	if n, _ := toNumber(next.Credits["remain"]); n != 100 {
		t.Fatal("incoming record mutated")
	}
	p.ApplyChanges([]*accounts.Record{incrementalRecord(t, "u", "cn", 80, 2, "management edit")}, nil)
	if n, _ := a.CreditRemain(); n != 80 {
		t.Fatalf("genuine credit refresh ignored: %v", n)
	}
	p.ApplyChanges(nil, []string{"u"})
	if p.Get("u") != nil || a.enabled.Load() || a.Inflight() != 1 || p.Get("other") != other {
		t.Fatal("delete did not disable retained pointer or lost unrelated account")
	}
}

func TestApplyChangesRealmReplacementAndSnapshotNoop(t *testing.T) {
	p := New([]*accounts.Record{incrementalRecord(t, "u", "cn", 100, 1, ""), incrementalRecord(t, "v", "cn", 100, 1, "")})
	a := p.Get("u")
	a.Acquire()
	p.ApplyChanges([]*accounts.Record{incrementalRecord(t, "u", "global", 100, 1, "")}, nil)
	if p.Get("u") == a || a.enabled.Load() || a.Inflight() != 1 || p.Get("u").Realm != "global" || len(p.snap.Load().byRealm["cn"]) != 1 {
		t.Fatal("realm replacement retained a schedulable old credential")
	}
	s := p.snap.Load()
	p.prefDirty.Store(false)
	p.SyncSnapshot([]*accounts.Record{incrementalRecord(t, "v", "cn", 100, 1, ""), incrementalRecord(t, "u", "global", 100, 1, "")})
	if p.snap.Load() != s || p.prefDirty.Load() {
		t.Fatal("equivalent full snapshot rebuilt preferences or membership")
	}
	v := p.Get("v")
	p.SyncSnapshot([]*accounts.Record{incrementalRecord(t, "u", "global", 100, 1, "")})
	if p.Len() != 1 || v.enabled.Load() {
		t.Fatal("authoritative snapshot did not remove absent UID")
	}
	p.SyncSnapshot(nil)
	if p.Len() != 0 {
		t.Fatal("empty authoritative snapshot retained accounts")
	}
}

func TestApplyChangesRestoresAuthoritativeFieldsDespiteSameBaseline(t *testing.T) {
	p := New([]*accounts.Record{incrementalRecord(t, "u", "cn", 100, 1, "shared")})
	a := p.Get("u")
	p.Update("u", func(r *accounts.Record) { r.Note = "unsaved local edit" })
	a.ConsumeCredit(10)
	p.ApplyChanges([]*accounts.Record{incrementalRecord(t, "u", "cn", 100, 1, "shared")}, nil)
	if a.Record().Note != "shared" {
		t.Fatal("equal baseline hid authoritative rollback of local field")
	}
	if n, _ := a.CreditRemain(); n != 90 {
		t.Fatal("rollback replenished locally consumed credits")
	}
}

func TestApplyChangesNoopTombstonesAndUpsertWins(t *testing.T) {
	p := New(nil)
	s := p.snap.Load()
	p.prefDirty.Store(false)
	p.ApplyChanges(nil, []string{"missing"})
	if p.snap.Load() != s || p.prefDirty.Load() {
		t.Fatal("unknown tombstone rebuilt empty pool")
	}
	p.ApplyChanges([]*accounts.Record{nil, incrementalRecord(t, "u", "cn", 100, 1, "")}, []string{"u"})
	if p.Get("u") == nil {
		t.Fatal("upsert did not win over same-batch tombstone")
	}
}

func TestMembershipRemovalAndReplacementDisableRetainedPointers(t *testing.T) {
	for _, operation := range []string{"remove", "replace"} {
		t.Run(operation, func(t *testing.T) {
			p := New([]*accounts.Record{incrementalRecord(t, "u", "cn", 100, 1, ""), incrementalRecord(t, "other", "cn", 100, 1, "")})
			old, other := p.Get("u"), p.Get("other")
			old.Acquire()
			old.NoteResult(false)
			if !old.Ready("", time.Now()) {
				t.Fatal("test account unexpectedly not ready")
			}
			if operation == "remove" {
				if !p.Remove("u") || p.Get("u") != nil {
					t.Fatal("membership removal failed")
				}
			} else {
				next := p.Add(incrementalRecord(t, "u", "cn", 100, 2, "new"))
				if next == old || p.Get("u") != next || !next.Enabled() {
					t.Fatal("replacement was not installed as a new enabled pointer")
				}
			}
			if old.Enabled() || old.Ready("", time.Now()) || old.Inflight() != 1 || old.Snapshot().Failures != 1 {
				t.Fatal("retained old pointer remained schedulable or lost in-flight state")
			}
			if p.Get("other") != other || !other.Enabled() {
				t.Fatal("unrelated account changed")
			}
		})
	}
}

func BenchmarkApplyChangesSingleExisting(b *testing.B) {
	recs := make([]*accounts.Record, 10000)
	for i := range recs {
		recs[i] = &accounts.Record{UID: time.Unix(int64(i), 0).String(), Enabled: true, Weight: 1}
	}
	p := New(recs)
	rec := *recs[0]
	updates := []*accounts.Record{&rec}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		next := *recs[0]
		next.Weight = 1 + i%2
		updates[0] = &next
		p.ApplyChanges(updates, nil)
	}
}
