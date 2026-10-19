package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func wait(t *testing.T, m *Manager, id string) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if j, _ := m.Get(id); j.State != Running {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return Job{}
}

func TestJobProcessesAllItemsAndCountsFailures(t *testing.T) {
	m := &Manager{Workers: 3}
	var seen atomic.Int32
	items := make([]string, 100)
	for i := range items {
		items[i] = fmt.Sprint(i)
	}
	j := m.Start("test", items, func(ctx context.Context, item string) error {
		seen.Add(1)
		if item == "7" || item == "42" {
			return errors.New("boom " + item)
		}
		return nil
	})
	if j.State != Running || j.Total != 100 || j.ID == "" {
		t.Fatalf("initial snapshot %+v", j)
	}
	done := wait(t, m, j.ID)
	if done.State != Done || done.Done != 100 || done.Failed != 2 || seen.Load() != 100 || len(done.Errors) != 2 {
		t.Fatalf("%+v", done)
	}
}

func TestParallelismIsBounded(t *testing.T) {
	m := &Manager{Workers: 4}
	var cur, peak atomic.Int32
	j := m.Start("p", make([]string, 40), func(ctx context.Context, _ string) error {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		cur.Add(-1)
		return nil
	})
	wait(t, m, j.ID)
	if peak.Load() > 4 || peak.Load() < 2 {
		t.Fatalf("peak concurrency %d, want 2..4", peak.Load())
	}
}

func TestCancel(t *testing.T) {
	m := &Manager{Workers: 1}
	var ran atomic.Int32
	j := m.Start("c", make([]string, 1000), func(ctx context.Context, _ string) error {
		ran.Add(1)
		time.Sleep(5 * time.Millisecond)
		return nil
	})
	time.Sleep(30 * time.Millisecond)
	if !m.Cancel(j.ID) {
		t.Fatal("cancel")
	}
	done := wait(t, m, j.ID)
	if done.State != Cancelled || done.Done >= 1000 || int(ran.Load()) != done.Done {
		t.Fatalf("%+v ran=%d", done, ran.Load())
	}
	if m.Cancel("nope") {
		t.Fatal("unknown id")
	}
}

func TestPanicInOneItemDoesNotKillTheJob(t *testing.T) {
	m := &Manager{}
	j := m.Start("x", []string{"a", "b", "c"}, func(ctx context.Context, item string) error {
		if item == "b" {
			panic("bad item")
		}
		return nil
	})
	done := wait(t, m, j.ID)
	if done.Done != 3 || done.Failed != 1 || done.State != Done {
		t.Fatalf("%+v", done)
	}
}

func TestProgressNotificationsAndListAndPrune(t *testing.T) {
	var notes atomic.Int32
	m := &Manager{Keep: 3, OnProgress: func(Job) { notes.Add(1) }}
	var last Job
	for i := 0; i < 5; i++ {
		last = m.Start("k", []string{"1"}, func(context.Context, string) error { return nil })
		wait(t, m, last.ID)
		time.Sleep(2 * time.Millisecond)
	}
	if notes.Load() < 5 {
		t.Errorf("every job must announce its final state, got %d notifications", notes.Load())
	}
	list := m.List()
	if len(list) != 3 || list[0].ID != last.ID {
		t.Fatalf("keep=3 newest first: %+v", list)
	}
}

func TestErrorListIsBounded(t *testing.T) {
	m := &Manager{Workers: 4}
	j := m.Start("e", make([]string, 500), func(context.Context, string) error { return errors.New("x") })
	done := wait(t, m, j.ID)
	if done.Failed != 500 || len(done.Errors) != maxErrors {
		t.Fatalf("failed=%d errors=%d", done.Failed, len(done.Errors))
	}
}
