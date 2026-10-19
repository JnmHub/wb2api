package usageest

import (
	"testing"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/store/accounts"
	"wb2api/internal/usagedb"
)

func testPool(t *testing.T, uids ...string) *pool.Pool {
	t.Helper()
	var recs []*accounts.Record
	exp := time.Now().Add(24 * time.Hour).Unix()
	for _, uid := range uids {
		recs = append(recs, accounts.FromMap(map[string]any{
			"uid": uid, "accessToken": "tok", "realm": "intl", "expiresAt": exp, "enabled": true,
		}, "", time.Now()))
	}
	return pool.New(recs)
}

func newDB(t *testing.T, now time.Time) *usagedb.DB {
	t.Helper()
	db, err := usagedb.Open(t.TempDir()+"/usage.db", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func addUsage(db *usagedb.DB, uid, model string, at time.Time, total int64) {
	db.Record(usagedb.Event{At: at, Account: uid, Model: model, Realm: "intl", Total: total, Outcome: "completed"})
}

func TestEstimateBudgetAndRemainingFromTheCapSample(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local)
	db := newDB(t, now)
	// Before the cap (inside the window): 100 tokens. The cap lands at now-2h and
	// says the window resets at now-1h; after the reset: 20 tokens.
	addUsage(db, "u1", "m1", now.Add(-3*time.Hour), 100)
	addUsage(db, "u1", "m1", now.Add(-30*time.Minute), 20)
	addUsage(db, "u2", "m1", now.Add(-time.Hour), 5)
	db.Flush()

	dir := t.TempDir()
	est := &Estimator{DB: db, Pool: testPool(t, "u1", "u2"), Dir: dir, Now: func() time.Time { return now }, TTL: time.Hour}
	est.NoteLimit("u1", "m1", "intl", now.Add(-time.Hour), now.Add(-2*time.Hour))
	p := est.Payload()
	if len(p.Rows) != 2 {
		t.Fatalf("rows: %+v", p.Rows)
	}
	if p.Rows[0].UID != "u1" {
		t.Fatalf("the smaller remaining quota sorts first: %+v", p.Rows)
	}
	u1 := p.Rows[0]
	if u1.Budget == nil || *u1.Budget != 100 {
		t.Fatalf("budget = %v, want the 100-token cap sample", u1.Budget)
	}
	if u1.Used != 20 || u1.Remaining == nil || *u1.Remaining != 80 {
		t.Fatalf("used=%d remaining=%v", u1.Used, u1.Remaining)
	}
	if u1.Cooling || u1.Estimated {
		t.Fatalf("a past cap is a real window: %+v", u1)
	}
	if p.Rows[1].UID != "u2" || p.Rows[1].Remaining == nil || *p.Rows[1].Remaining != 95 {
		t.Fatalf("the budget is shared per model: %+v", p.Rows[1])
	}
	if len(p.Models) != 1 || p.Models[0].TotalRemaining == nil || *p.Models[0].TotalRemaining != 175 {
		t.Fatalf("models: %+v", p.Models)
	}
}

func TestCoolingRowHasZeroRemainingAndNoWeight(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local)
	db := newDB(t, now)
	addUsage(db, "u1", "m1", now.Add(-time.Hour), 50)
	addUsage(db, "u1", "m1", now.Add(-2*time.Hour), 100)
	db.Flush()
	est := &Estimator{DB: db, Pool: testPool(t, "u1"), Dir: t.TempDir(), Now: func() time.Time { return now }, TTL: time.Hour}
	est.NoteLimit("u1", "m1", "intl", now.Add(-3*time.Hour), now.Add(-5*time.Hour)) // an older window
	est.NoteLimit("u1", "m1", "intl", now.Add(3*time.Hour), now.Add(-time.Minute))  // cooling now
	row := est.Payload().Rows[0]
	if !row.Cooling || row.Remaining == nil || *row.Remaining != 0 {
		t.Fatalf("%+v", row)
	}
	if w := est.ScheduleWeights(); len(w) != 0 {
		t.Fatalf("a cooling pair must not be preferred: %v", w)
	}
}

func TestScheduleWeightsBands(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local)
	db := newDB(t, now)
	// One cap establishes a 100-token budget for intl/m1.
	addUsage(db, "cap", "m1", now.Add(-3*time.Hour), 100)
	db.Flush()
	addUsage(db, "near", "m1", now.Add(-time.Hour), 97)  // 3% left -> weight 4
	addUsage(db, "mid", "m1", now.Add(-2*time.Hour), 90) // 10% left -> weight 3
	addUsage(db, "ok", "m1", now.Add(-time.Hour), 10)    // 90% left -> no weight
	db.Flush()
	est := &Estimator{DB: db, Pool: testPool(t, "near", "mid", "ok", "cap"), Dir: t.TempDir(), Now: func() time.Time { return now }, TTL: time.Hour}
	est.NoteLimit("cap", "m1", "intl", now.Add(-time.Hour), now.Add(-2*time.Hour))
	w := est.ScheduleWeights()["m1"]
	if w["near"] != 4 || w["mid"] != 3 {
		t.Fatalf("weights: %v", w)
	}
	if _, ok := w["ok"]; ok {
		t.Fatalf("plenty left must not be weighted: %v", w)
	}
}

func TestPayloadIsCachedForTheTTL(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local)
	db := newDB(t, now)
	est := &Estimator{DB: db, Pool: testPool(t, "u1"), Dir: t.TempDir(), Now: func() time.Time { return now }, TTL: time.Hour}
	a := est.Payload()
	addUsage(db, "u1", "m1", now.Add(-time.Hour), 10)
	db.Flush()
	b := est.Payload()
	if a.ETag != b.ETag {
		t.Fatal("the TTL must serve the cached estimate")
	}
}
