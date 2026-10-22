package usagedb

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
)

var ErrAdmissionBusy = errors.New("usagedb: local admission queue wait timed out; event not accepted")

const localQueueCapacity = 64
const localBatchBytes = 4 << 20

type localAdmission struct {
	full, minimal []byte
	detail        bool
	barrier       bool
	ack           chan error
}

func (a *localAdmission) bytes() int { return len(a.full) + len(a.minimal) }

// enqueue serializes only bounded encoding and queue insertion, NEVER fsync.
// Both gate and channel waits observe timeout and the independent Close fence.
func (d *DB) enqueueLocal(e *Event, barrier bool) (chan error, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d.writeTimeout)
	defer cancel()
	select {
	case <-d.admissionClosing:
		d.dropped.Add(1)
		return nil, errNoDB
	default:
	}
	select {
	case d.admissionGate <- struct{}{}:
	case <-d.admissionClosing:
		d.dropped.Add(1)
		return nil, errNoDB
	case <-ctx.Done():
		return nil, ErrAdmissionBusy
	}
	defer func() { <-d.admissionGate }()
	select {
	case <-d.admissionClosing:
		d.dropped.Add(1)
		return nil, errNoDB
	default:
	}
	a := &localAdmission{barrier: barrier, ack: make(chan error, 1)}
	if e != nil {
		if s := d.readiness.Load(); s != nil && s.health.DurabilityError != "" {
			return nil, s.err
		}
		var err error
		a.full, a.minimal, a.detail, err = d.encodeAdmission(*e)
		if err != nil {
			return nil, err
		}
	}
	select {
	case d.admissionQ <- a:
		return a.ack, nil
	case <-d.admissionClosing:
		d.dropped.Add(1)
		return nil, errNoDB
	case <-ctx.Done():
		return nil, ErrAdmissionBusy
	}
}

func (d *DB) encodeAdmission(e Event) (full, minimal []byte, detail bool, err error) {
	if math.IsNaN(e.Credit) || math.IsInf(e.Credit, 0) {
		return nil, nil, false, errors.New("usagedb: non-finite credit")
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	if len(e.Model)+len(e.Account)+len(e.Realm)+len(e.KeyID)+len(e.Outcome) > d.maxEventBytes/2 {
		return nil, nil, false, ErrEventTooLarge
	}
	for _, v := range []string{e.Model, e.Account, e.Realm, e.KeyID, e.Outcome} {
		if strings.ContainsRune(v, 0) {
			return nil, nil, false, errors.New("usagedb: NUL in accounting dimension")
		}
	}
	detail = len(e.Error)+len(e.Extra)+len(e.Effort) <= d.maxEventBytes/2 && !strings.ContainsRune(e.Error, 0) && !strings.ContainsRune(e.Effort, 0)
	stripped := e
	stripped.Error = ""
	stripped.Extra = nil
	stripped.Effort = ""
	minimal, err = json.Marshal(durableEvent{Event: stripped, Day: localDay(e.At)})
	if err != nil {
		return nil, nil, false, err
	}
	if len(minimal) > d.maxEventBytes {
		return nil, nil, false, ErrEventTooLarge
	}
	if detail {
		full, err = json.Marshal(durableEvent{Event: e, Day: localDay(e.At)})
		if err != nil || len(full) > d.maxEventBytes {
			detail = false
			full = nil
		}
	}
	if !detail {
		return nil, minimal, false, nil
	}
	return full, minimal, true, nil
}

func (d *DB) localWriter() {
	defer close(d.admissionDone)
	var carry *localAdmission
	for {
		var first *localAdmission
		if carry != nil {
			first = carry
			carry = nil
		} else {
			var ok bool
			first, ok = <-d.admissionQ
			if !ok {
				return
			}
		}
		if first.barrier {
			d.recordMu.RLock()
			err := d.durabilityErr
			d.recordMu.RUnlock()
			first.ack <- err
			continue
		}
		group := []*localAdmission{first}
		bytes := first.bytes()
	drain:
		for len(group) < batchMax && bytes < localBatchBytes {
			select {
			case next, ok := <-d.admissionQ:
				if !ok {
					break drain
				}
				if next.barrier || bytes+next.bytes() > localBatchBytes {
					carry = next
					break drain
				}
				group = append(group, next)
				bytes += next.bytes()
			default:
				break drain
			}
		}
		d.commitLocal(group)
	}
}

func (d *DB) commitLocal(group []*localAdmission) {
	d.recordMu.Lock()
	results := make([]error, len(group))
	payloads := make([][]byte, len(group))
	details := make([]bool, len(group))
	count, bytes := d.outboxCount, d.outboxBytes
	var omitted int64
	for i, a := range group {
		if d.durabilityErr != nil {
			results[i] = d.durabilityErr
			continue
		}
		detail := a.detail
		if count >= int64(d.queueCap) {
			detail = false
		} else if d.overflow == OverflowSample && count*4 >= int64(d.queueCap)*3 && d.sampleEvery > 1 {
			sampled := d.sampleTick.Add(1)%d.sampleEvery == 0
			detail = detail && sampled
		}
		p := a.minimal
		if detail {
			p = a.full
		}
		if int64(len(p)) > d.outboxMax-bytes {
			results[i] = ErrOutboxFull
			continue
		}
		payloads[i] = p
		details[i] = detail
		count++
		bytes += int64(len(p))
		if !detail {
			omitted++
		}
	}
	var storeErr error
	if count > d.outboxCount {
		ctx, cancel := context.WithTimeout(d.writerCtx, d.writeTimeout)
		tx, err := d.outbox.BeginTx(ctx, nil)
		if err == nil {
			for i, p := range payloads {
				if p == nil {
					continue
				}
				if _, err = tx.ExecContext(ctx, "INSERT INTO events(payload,detail) VALUES(?,?)", p, b2i(details[i])); err != nil {
					break
				}
			}
			if err == nil {
				err = tx.Commit()
			} else {
				_ = tx.Rollback()
			}
		}
		cancel()
		if err != nil {
			storeErr = d.localFailureLocked(err)
		} else {
			d.localCommits.Add(1)
			d.outboxCount = count
			d.outboxBytes = bytes
			d.pending.Store(count)
			d.dropped.Add(omitted)
			d.publishReadinessLocked()
		}
	}
	d.recordMu.Unlock()
	for i, a := range group {
		if results[i] != nil {
			a.ack <- results[i]
		} else {
			a.ack <- storeErr
		}
	}
}
