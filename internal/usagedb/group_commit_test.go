package usagedb

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestGroupCommitAcknowledgesOnlyCommittedBatch(t *testing.T) {
	d := open(t)
	d.recordMu.Lock()
	acks := make([]chan error, 64)
	for i := range acks {
		var err error
		e := retryEvent("group")
		acks[i], err = d.enqueueLocal(&e, false)
		if err != nil {
			d.recordMu.Unlock()
			t.Fatal(err)
		}
	}
	for _, ack := range acks {
		select {
		case err := <-ack:
			d.recordMu.Unlock()
			t.Fatalf("ack before transaction possible: %v", err)
		default:
		}
	}
	d.recordMu.Unlock()
	for _, ack := range acks {
		if err := <-ack; err != nil {
			t.Fatal(err)
		}
	}
	if n := d.localCommits.Load(); n > 2 || n < 1 {
		t.Fatalf("64 queued records required %d FULL commits, want1..2", n)
	}
	if err := d.Flush(); err != nil {
		t.Fatal(err)
	}
	checkRetryTotals(t, d, 64)
}

func TestGroupCommitTransactionFailureResolvesAllWaiters(t *testing.T) {
	d := open(t)
	if _, err := d.outbox.Exec("CREATE TRIGGER fail_group BEFORE INSERT ON events WHEN NEW.seq<>0 BEGIN SELECT RAISE(ABORT,'group storage fault'); END"); err != nil {
		t.Fatal(err)
	}
	d.recordMu.Lock()
	acks := make([]chan error, 32)
	for i := range acks {
		e := retryEvent("group")
		var err error
		acks[i], err = d.enqueueLocal(&e, false)
		if err != nil {
			d.recordMu.Unlock()
			t.Fatal(err)
		}
	}
	d.recordMu.Unlock()
	for _, ack := range acks {
		select {
		case err := <-ack:
			if err == nil {
				t.Fatal("failed tx acknowledged success")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("stranded group caller")
		}
	}
	if d.Ready() == nil || d.Health().DurabilityError == "" {
		t.Fatal("storage failure not latched")
	}
	if err := d.Flush(); err == nil {
		t.Fatal("Flush concealed local durability failure")
	}
	var n int
	if err := d.outbox.QueryRow("SELECT COUNT(*) FROM events").Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows=%d %v", n, err)
	}
}

func TestGroupFlushBarrierAndBudgetPrefix(t *testing.T) {
	d, err := OpenWithOptions(filepath.Join(t.TempDir(), "usage.db"), Options{OutboxMaxBytes: 1400, QueueSize: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.recordMu.Lock()
	acks := make([]chan error, 8)
	for i := range acks {
		e := retryEvent("prefix")
		acks[i], err = d.enqueueLocal(&e, false)
		if err != nil {
			d.recordMu.Unlock()
			t.Fatal(err)
		}
	}
	barrier, err := d.enqueueLocal(nil, true)
	if err != nil {
		d.recordMu.Unlock()
		t.Fatal(err)
	}
	select {
	case <-barrier:
		d.recordMu.Unlock()
		t.Fatal("barrier passed uncommitted records")
	default:
	}
	d.recordMu.Unlock()
	accepted := int64(0)
	rejected := false
	for _, ack := range acks {
		err = <-ack
		if err == nil {
			if rejected {
				t.Fatal("same-size budget accepted after rejected prefix")
			}
			accepted++
		} else if errors.Is(err, ErrOutboxFull) {
			rejected = true
		} else {
			t.Fatal(err)
		}
	}
	if err = <-barrier; err != nil {
		t.Fatal(err)
	}
	if accepted == 0 || !rejected {
		t.Fatalf("accepted=%d rejected=%v", accepted, rejected)
	}
	if err = d.Flush(); err != nil {
		t.Fatal(err)
	}
	checkRetryTotals(t, d, accepted)
}

func TestGroupFullQueueCloseCancellationResolvesAll(t *testing.T) {
	d, err := OpenWithOptions(filepath.Join(t.TempDir(), "usage.db"), Options{WriteTimeout: 30 * time.Millisecond, CloseTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	d.recordMu.Lock()
	var acks []chan error
	for range 200 {
		e := retryEvent("queue")
		ack, eerr := d.enqueueLocal(&e, false)
		if errors.Is(eerr, ErrAdmissionBusy) {
			break
		}
		if eerr != nil {
			d.recordMu.Unlock()
			t.Fatal(eerr)
		}
		acks = append(acks, ack)
	}
	if len(d.admissionQ) != localQueueCapacity {
		d.recordMu.Unlock()
		t.Fatalf("queue len=%d", len(d.admissionQ))
	}
	if d.Ready() != nil {
		d.recordMu.Unlock()
		t.Fatal("queue timeout latched")
	}
	closed := make(chan error, 1)
	go func() { closed <- d.Close() }()
	<-d.admissionClosing
	<-d.writerCtx.Done()
	if d.Ready() == nil {
		d.recordMu.Unlock()
		t.Fatal("Close not fenced")
	}
	d.recordMu.Unlock()
	for _, ack := range acks {
		select {
		case err = <-ack:
			if err == nil {
				t.Fatal("canceled group acknowledged success")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Close abandoned queued caller")
		}
	}
	select {
	case err = <-closed:
		if err == nil {
			t.Fatal("canceled Close concealed failure")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close abandoned drain")
	}
	if d.Health().PendingEvents != 0 {
		t.Fatal(d.Health())
	}
}

func TestGroupAdmissionTimeoutDoesNotLatchAndCloseUnblocks(t *testing.T) {
	d, err := OpenWithOptions(filepath.Join(t.TempDir(), "usage.db"), Options{WriteTimeout: 30 * time.Millisecond, CloseTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	d.admissionGate <- struct{}{}
	if err = d.Record(retryEvent("busy")); !errors.Is(err, ErrAdmissionBusy) {
		t.Fatalf("busy=%v", err)
	}
	if d.Ready() != nil {
		t.Fatal("transient busy latched readiness")
	}
	result := make(chan error, 1)
	go func() { result <- d.Record(retryEvent("closing")) }()
	closed := make(chan error, 1)
	go func() { closed <- d.Close() }()
	select {
	case err = <-result:
		if !errors.Is(err, errNoDB) {
			t.Fatalf("closing Record=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close fence didn't unblock producer")
	}
	if d.Ready() == nil || d.Health().Ready {
		t.Fatal("closing not promptly unready")
	}
	<-d.admissionGate
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close stranded")
	}
}
