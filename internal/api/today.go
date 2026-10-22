package api

import (
	"sync"
	"time"

	"wb2api/internal/usagedb"
)

// DayStat is one account's consumption today.
type DayStat struct {
	Requests int64
	Tokens   int64
	Credit   float64
}

// Today keeps today's per-account consumption in memory for list views. Daily
// guards independently read database rollups. It rolls over at local midnight
// and is seeded once before serving requests, including recovered local outbox
// events not yet delivered to the sink.
type Today struct {
	Now func() time.Time

	mu  sync.RWMutex
	day string
	per map[string]*DayStat
}

func (t *Today) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t *Today) rollLocked(now time.Time) {
	day := now.In(time.Local).Format("2006-01-02")
	if t.per == nil || day != t.day {
		t.day = day
		t.per = map[string]*DayStat{}
	}
}

// Add counts one finished request.
func (t *Today) Add(ev usagedb.Event) {
	if ev.Account == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollLocked(t.now())
	if ev.At.In(time.Local).Format("2006-01-02") != t.day {
		return // a late event from yesterday
	}
	s := t.per[ev.Account]
	if s == nil {
		s = &DayStat{}
		t.per[ev.Account] = s
	}
	s.Requests++
	s.Tokens += ev.Total
	s.Credit += ev.Credit
}

// Get returns today's numbers for uid.
func (t *Today) Get(uid string) DayStat {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.per == nil || t.day != t.now().In(time.Local).Format("2006-01-02") {
		return DayStat{}
	}
	if s := t.per[uid]; s != nil {
		return *s
	}
	return DayStat{}
}

// Seed loads one startup snapshot of delivered and locally pending totals.
// Call before concurrent Add/Get; this is not a periodic reconciliation API.
// Query errors leave existing counters unchanged rather than fabricating zero.
func (t *Today) Seed(db *usagedb.DB) error {
	now := t.now()
	dc, err := db.DayIncludingPending(usagedb.LocalMidnight(now))
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollLocked(t.now())
	for uid, tok := range dc.TokensByAccount {
		t.per[uid] = &DayStat{Requests: dc.RequestsByAccount[uid], Tokens: tok, Credit: dc.CreditsByAccount[uid]}
	}
	return nil
}
