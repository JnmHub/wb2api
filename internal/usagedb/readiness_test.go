package usagedb

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestReadinessDoesNotWaitForDurableRecordMutex(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "usage.db"), 8)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.recordMu.Lock()
	// Keeping the mutex held until the call completes is the assertion. The
	// generous timeout only prevents a broken implementation deadlocking tests;
	// this is synchronization evidence, not a latency benchmark.
	done := make(chan error, 1)
	go func() {
		if err := d.Ready(); err != nil {
			done <- err
			return
		}
		if !d.Health().Ready {
			done <- errors.New("unexpected unready health")
			return
		}
		done <- nil
	}()
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		err = errors.New("Ready/Health blocked behind recordMu")
	}
	d.recordMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}

func TestReadinessPublishesAdmissionAckFailureClose(t *testing.T) {
	var uninitialized DB
	if uninitialized.Ready() == nil || uninitialized.Health().Ready {
		t.Fatal("uninitialized snapshot admitted")
	}
	// Freeze dispatcher so the payload fills a deliberately small logical budget.
	d, err := OpenWithOptions(filepath.Join(t.TempDir(), "usage.db"), Options{OutboxMaxBytes: 1800, MaxEventBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	failModel(t, d, "bad")
	if d.Ready() != nil {
		t.Fatal("open not ready")
	}
	for d.Ready() == nil {
		if err = d.Record(retryEvent("bad")); err != nil {
			t.Fatalf("Record before ready budget: %v", err)
		}
	}
	h := d.Health()
	if !errors.Is(d.Ready(), ErrOutboxFull) || h.Ready || h.PendingEvents == 0 || h.PendingBytes == 0 {
		t.Fatalf("full health=%+v ready=%v", h, d.Ready())
	}
	if _, err = d.sql.Exec("DROP TRIGGER reject_retry"); err != nil {
		t.Fatal(err)
	}
	if err = d.Flush(); err != nil {
		t.Fatal(err)
	}
	h = d.Health()
	if d.Ready() != nil || !h.Ready || h.PendingEvents != 0 || h.PendingBytes != 0 {
		t.Fatalf("ack health=%+v ready=%v", h, d.Ready())
	}
	injected := errors.New("injected local durability failure")
	d.localFailure(injected)
	if !errors.Is(d.Ready(), injected) || d.Health().Ready || d.Health().DurabilityError == "" {
		t.Fatalf("failure health=%+v ready=%v", d.Health(), d.Ready())
	}
	// A subsequent ack-style publication must not undo the sticky failure.
	d.recordMu.Lock()
	d.publishReadinessLocked()
	d.recordMu.Unlock()
	if !errors.Is(d.Ready(), injected) {
		t.Fatal("failure reverted")
	}
	_ = d.Close()
	if !errors.Is(d.Ready(), errNoDB) || d.Health().Ready {
		t.Fatalf("closed health=%+v ready=%v", d.Health(), d.Ready())
	}
}
