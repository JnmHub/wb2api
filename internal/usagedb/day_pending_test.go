package usagedb

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestDayIncludingPendingCountsReceiptAndUnconfirmedRowsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	d, err := Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		close(d.admissionQ)
		<-d.admissionDone
		d.writerCancel()
		d.outbox.Close()
		unlockOutbox(d.outboxLock)
		d.sql.Close()
	}()
	// Freeze dispatcher without closing handles. Test acknowledgement windows.
	close(d.stop)
	<-d.done
	if err = d.Record(retryEvent("m")); err != nil {
		t.Fatal(err)
	}
	u, err := d.loadUnit(context.Background(), 1<<62)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.writeBatch(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	// First is committed at sink but its local row is deliberately unacknowledged.
	// Second exists only locally and omits details under QueueSize=1 pressure.
	if err = d.Record(retryEvent("m")); err != nil {
		t.Fatal(err)
	}
	past := retryEvent("m")
	past.At = time.Now().AddDate(0, 0, -2)
	if err = d.Record(past); err != nil {
		t.Fatal(err)
	}
	for _, start := range []time.Time{LocalMidnight(time.Now()), time.Now().Add(-time.Hour)} {
		got, err := d.DayIncludingPending(start)
		if err != nil {
			t.Fatal(err)
		}
		if got.RequestsByAccount["a"] != 2 || got.TokensByAccount["a"] != 14 || got.CreditsByAccount["a"] != 0.5 || got.TokensByAccountModel["a"]["m"] != 14 {
			t.Fatalf("counters=%+v", got)
		}
	}
	// Draining all rows changes their delivery state, not the seeded total.
	if err = d.drainOutbox(true); err != nil {
		t.Fatal(err)
	}
	got, err := d.DayIncludingPending(LocalMidnight(time.Now()))
	if err != nil || got.TokensByAccount["a"] != 14 {
		t.Fatalf("delivered=%+v %v", got, err)
	}
}
