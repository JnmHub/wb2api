package usageest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"
)

// initEvents imports the legacy append log once with O(maxEvents) memory and
// compacts it asynchronously. Retain exactly the legacy newest-1000 semantics:
// time-based pruning would silently remove still-useful cap/reset samples.
func (e *Estimator) initEvents() {
	e.evInit.Do(func() {
		var events []limitEvent
		if e.Dir != "" {
			f, err := os.Open(e.eventsPath())
			if err == nil {
				ring := make([]limitEvent, maxEvents)
				n := 0
				sc := bufio.NewScanner(f)
				sc.Buffer(make([]byte, 64*1024), 1<<20)
				for sc.Scan() {
					var ev limitEvent
					if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Account == "" || ev.Model == "" || ev.At <= 0 {
						continue
					}
					ring[n%maxEvents] = ev
					n++
				}
				err = sc.Err()
				_ = f.Close()
				if err != nil {
					e.evLoadErr = err
					e.logger().Warn("remaining estimate: cannot read all limit events", "err", err)
				}
				count := n
				if count > maxEvents {
					count = maxEvents
				}
				events = make([]limitEvent, count)
				start := n - count
				for i := range events {
					events[i] = ring[(start+i)%maxEvents]
				}
				// Do not rewrite a partially read file: that could destroy valid estimates.
				if err == nil {
					e.evDirty = true
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				e.evLoadErr = err
				e.logger().Warn("remaining estimate: cannot read limit events", "err", err)
			}
		}
		e.evMu.Lock()
		e.events = events
		if e.evDirty {
			e.startEventWorkerLocked()
		}
		e.evMu.Unlock()
	})
}

// NoteLimit only updates bounded memory after Start has preloaded the log.
// Persistence never holds the event mutex during filesystem I/O and no longer
// takes the estimate build mutex across I/O. Process crashes may lose pending
// best-effort observations; this is not a durable quota/accounting ledger.
func (e *Estimator) NoteLimit(uid, model, realmName string, reset, at time.Time) {
	if uid == "" || model == "" || reset.IsZero() {
		return
	}
	e.initEvents()
	ev := limitEvent{At: float64(at.UnixNano()) / 1e9, Account: uid, Model: model, Realm: realmName, Reset: float64(reset.UnixNano()) / 1e9}
	e.evMu.Lock()
	if len(e.events) >= maxEvents {
		copy(e.events, e.events[len(e.events)-maxEvents+1:])
		e.events = e.events[:maxEvents-1]
	}
	e.events = append(e.events, ev)
	e.evDirty = true
	e.evVersion++
	e.startEventWorkerLocked()
	e.evMu.Unlock()
	e.Invalidate()
}

func (e *Estimator) loadEvents() []limitEvent {
	e.initEvents()
	e.evMu.Lock()
	defer e.evMu.Unlock()
	return append([]limitEvent(nil), e.events...)
}
func (e *Estimator) startEventWorkerLocked() {
	if e.evWorker || e.Dir == "" || e.evLoadErr != nil {
		return
	}
	e.evWorker = true
	e.evDone = make(chan struct{})
	e.evErr = nil
	go e.persistEvents()
}
func (e *Estimator) persistEvents() {
	for {
		e.evMu.Lock()
		version := e.evVersion
		events := append([]limitEvent(nil), e.events...)
		e.evMu.Unlock()
		err := e.writeEvents(events)
		e.evMu.Lock()
		if err != nil {
			e.evErr = err
			e.evWorker = false
			close(e.evDone)
			e.evMu.Unlock()
			e.logger().Warn("remaining estimate: cannot persist limit events", "err", err)
			return
		}
		if e.evVersion == version {
			e.evDirty = false
			e.evWorker = false
			close(e.evDone)
			e.evMu.Unlock()
			return
		}
		e.evMu.Unlock()
	}
}

// FlushEvents waits for pending best-effort persistence, respecting the caller's
// deadline. A filesystem operation itself is not forcibly cancelable in Go.
func (e *Estimator) FlushEvents(ctx context.Context) error {
	e.initEvents()
	e.evMu.Lock()
	if e.evLoadErr != nil {
		err := e.evLoadErr
		e.evMu.Unlock()
		return err
	}
	if !e.evDirty || e.Dir == "" {
		e.evMu.Unlock()
		return nil
	}
	e.startEventWorkerLocked()
	done := e.evDone
	e.evMu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
	}
	e.evMu.Lock()
	err := e.evErr
	e.evMu.Unlock()
	return err
}
func (e *Estimator) writeEvents(events []limitEvent) error {
	if err := os.MkdirAll(e.Dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(e.Dir, ".limit-events-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	enc := json.NewEncoder(f)
	for _, ev := range events {
		if err = enc.Encode(ev); err != nil {
			break
		}
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, e.eventsPath())
}
