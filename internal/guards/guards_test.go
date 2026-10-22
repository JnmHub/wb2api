package guards

import (
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/store/accounts"
	"wb2api/internal/store/settings"
	"wb2api/internal/usagedb"
)

type usageFunc func(time.Time) ([]usagedb.DayUse, error)

func (f usageFunc) DayUsage(day time.Time) ([]usagedb.DayUse, error) { return f(day) }

func guardScanner(t *testing.T) (*Scanner, *pool.Account) {
	t.Helper()
	p := pool.New([]*accounts.Record{{UID: "account", Realm: "intl", AccessToken: "token", Credits: map[string]any{"remain": float64(10)}}})
	s := &Scanner{Pool: p, Settings: settings.Open(t.TempDir()), TTL: time.Hour,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return s, p.All()[0]
}

func setLimit(t *testing.T, s *Scanner, key string, n int) {
	t.Helper()
	if _, err := s.Settings.SetLimit(key, "global", &n); err != nil {
		t.Fatal(err)
	}
}

func TestSameDayUsageFailurePreservesKnownCountsAndCurrentSettings(t *testing.T) {
	s, a := guardScanner(t)
	for _, key := range []string{settings.LimitDailyToken, settings.LimitDailyCredit, settings.LimitModelDailyToken} {
		setLimit(t, s, key, 100)
	}
	fail := false
	s.Usage = usageFunc(func(time.Time) ([]usagedb.DayUse, error) {
		if fail {
			return nil, errors.New("usage unavailable")
		}
		return []usagedb.DayUse{{Account: a.UID, Model: "paid", Tokens: 150, Credit: 150}}, nil
	})
	free := "free-before"
	s.FreeModels = func(string) map[string]bool { return map[string]bool{free: true} }
	s.Apply(true)
	_, token, credit := a.Guards()
	if !token || !credit || !a.ModelTokenBlocked("paid") || a.CreditBlocked(free) {
		t.Fatal("initial quota guards missing")
	}
	fail, free = true, "free-after"
	setLimit(t, s, settings.LimitReserveCredits, 20)
	s.Apply(true)
	reserve, token, credit := a.Guards()
	if !reserve || !token || !credit || !a.ModelTokenBlocked("paid") {
		t.Fatal("same-day failure cleared known daily/model/credit or lost fresh reserve")
	}
	if a.CreditBlocked("free-after") || !a.CreditBlocked("free-before") {
		t.Fatal("cached counts did not use fresh free-model exemptions")
	}
	// New limits must be evaluated against cached numbers, not frozen flags.
	setLimit(t, s, settings.LimitDailyToken, 200)
	setLimit(t, s, settings.LimitModelDailyToken, 200)
	s.Apply(false)
	_, token, credit = a.Guards()
	if token || !credit || a.ModelTokenBlocked("paid") {
		t.Fatal("changed limits not evaluated against last-good counts")
	}
	setLimit(t, s, settings.LimitDailyCredit, 0)
	setLimit(t, s, settings.LimitReserveCredits, 0)
	s.Apply(false)
	reserve, token, credit = a.Guards()
	if reserve || token || credit || a.CreditBlocked("paid") || a.ModelTokenBlocked("paid") {
		t.Fatal("disabled settings did not clear guards during outage")
	}
}

func TestUsageFailureDoesNotReusePreviousDayWithinTTL(t *testing.T) {
	s, a := guardScanner(t)
	setLimit(t, s, settings.LimitDailyToken, 100)
	now := time.Date(2026, 10, 22, 23, 59, 59, 0, time.Local)
	s.now = func() time.Time { return now }
	calls := 0
	s.Usage = usageFunc(func(day time.Time) ([]usagedb.DayUse, error) {
		calls++
		if calls > 1 {
			return nil, errors.New("new day unavailable")
		}
		return []usagedb.DayUse{{Account: a.UID, Model: "m", Tokens: 150}}, nil
	})
	s.Apply(false)
	_, blocked, _ := a.Guards()
	if !blocked {
		t.Fatal("initial quota guard missing")
	}
	now = now.Add(2 * time.Second)
	s.Apply(false)
	_, blocked, _ = a.Guards()
	if blocked || s.counts != nil || calls != 2 {
		t.Fatalf("yesterday reused inside TTL: blocked=%v counts=%v calls=%d", blocked, s.counts, calls)
	}
}

func TestUsageColdFailureRemainsFailOpenButReserveApplies(t *testing.T) {
	s, a := guardScanner(t)
	for _, key := range []string{settings.LimitDailyToken, settings.LimitDailyCredit, settings.LimitModelDailyToken, settings.LimitReserveCredits} {
		setLimit(t, s, key, 100)
	}
	s.Usage = usageFunc(func(time.Time) ([]usagedb.DayUse, error) { return nil, errors.New("cold failure") })
	s.Apply(true)
	reserve, token, credit := a.Guards()
	if !reserve || token || credit || a.ModelTokenBlocked("m") {
		t.Fatal("cold unknown usage should fail open without bypassing reserve")
	}
}

func TestConcurrentScansSerializeWithoutHoldingFastPathMutex(t *testing.T) {
	s, a := guardScanner(t)
	setLimit(t, s, settings.LimitDailyToken, 100)
	started, unblock := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(unblock) }) }
	defer release()
	var calls atomic.Int32
	secondStarted := make(chan struct{})
	s.Usage = usageFunc(func(time.Time) ([]usagedb.DayUse, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-unblock
			return []usagedb.DayUse{{Account: a.UID, Model: "m", Tokens: 150}}, nil
		}
		close(secondStarted)
		return nil, nil
	})
	firstDone := make(chan struct{})
	go func() { s.Apply(true); close(firstDone) }()
	<-started
	// Inside TTL, requests keep using published guards and must not queue
	// behind the usage I/O/scan serialization lock.
	fastDone := make(chan struct{})
	go func() { s.Apply(false); close(fastDone) }()
	select {
	case <-fastDone:
	case <-time.After(time.Second):
		release()
		<-firstDone
		<-fastDone
		t.Fatal("usage I/O held fast-path gate")
	}
	secondDone := make(chan struct{})
	go func() { s.Apply(true); close(secondDone) }()
	select {
	case <-secondStarted:
		release()
		<-firstDone
		<-secondDone
		t.Fatal("newer scan overlapped older blocked usage fold")
	case <-time.After(30 * time.Millisecond):
	}
	release()
	<-firstDone
	<-secondDone
	_, blocked, _ := a.Guards()
	if calls.Load() != 2 || blocked {
		t.Fatalf("old scan restored stale restriction: calls=%d blocked=%v", calls.Load(), blocked)
	}
}

func TestSettingsChangeDuringBlockedUsageReadUsesLatestLimits(t *testing.T) {
	s, a := guardScanner(t)
	setLimit(t, s, settings.LimitDailyToken, 100)
	s.Usage = usageFunc(func(time.Time) ([]usagedb.DayUse, error) {
		return []usagedb.DayUse{{Account: a.UID, Model: "m", Tokens: 150}}, nil
	})
	s.Apply(true)
	started, unblock := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(unblock) }) }
	defer release()
	s.Usage = usageFunc(func(time.Time) ([]usagedb.DayUse, error) {
		close(started)
		<-unblock
		return nil, errors.New("usage unavailable")
	})
	done := make(chan struct{})
	go func() { s.Apply(true); close(done) }()
	<-started
	setLimit(t, s, settings.LimitDailyToken, 0)
	release()
	<-done
	_, blocked, _ := a.Guards()
	if blocked {
		t.Fatal("settings disabled during blocked read but old cap was restored")
	}
}

func TestUsageFoldCrossingMidnightDoesNotPublishOldDayBlocks(t *testing.T) {
	s, a := guardScanner(t)
	setLimit(t, s, settings.LimitDailyToken, 100)
	now := time.Date(2026, 10, 22, 23, 59, 59, 0, time.Local)
	s.now = func() time.Time { return now }
	s.Usage = usageFunc(func(time.Time) ([]usagedb.DayUse, error) {
		now = now.Add(2 * time.Second)
		return []usagedb.DayUse{{Account: a.UID, Model: "m", Tokens: 150}}, nil
	})
	s.Apply(true)
	_, blocked, _ := a.Guards()
	if blocked || s.counts != nil {
		t.Fatal("fold crossed midnight and published yesterday's usage")
	}
}
