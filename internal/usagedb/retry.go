package usagedb

import (
	"context"
	"fmt"
)

// A durable source is exclusively owned by one process. Its monotonically
// increasing sequence is persisted in the outbox, not regenerated on retry.
// Sink high-water marks commit in the SAME transaction as accounting facts,
// rollups and optional details. Receipt storage is O(sources), not O(events).
// Never restore an OLD outbox snapshot after acknowledging newer events, copy
// it to another writer, or delete usage_sources while its outbox can replay.
type writeUnit struct {
	id      string
	seqs    []int64
	events  []Event
	details []bool
	bytes   int64
}

func (d *DB) pendingError() error {
	if n := d.pending.Load(); n > 0 {
		return fmt.Errorf("usagedb: %d durable events pending retry: %s", n, d.LastError())
	}
	return nil
}

// drainOutbox snapshots the last accepted sequence; producers cannot extend a
// Flush indefinitely. A periodic tick handles at most one bounded batch.
func (d *DB) drainOutbox(all bool) error {
	var until int64
	if err := d.outbox.QueryRowContext(d.writerCtx, "SELECT COALESCE(MAX(seq),0) FROM events").Scan(&until); err != nil {
		return d.localFailure(err)
	}
	for {
		u, err := d.loadUnit(d.writerCtx, until)
		if err != nil {
			return d.localFailure(err)
		}
		if len(u.events) == 0 {
			return nil
		}
		ctx, cancel := context.WithTimeout(d.writerCtx, d.writeTimeout)
		d.batches.Add(1)
		err = d.writeBatch(ctx, u)
		cancel()
		if err != nil {
			d.failures.Add(1)
			d.lastErr.Store(err.Error())
			d.sinkErr.Store(err.Error())
			return d.pendingError()
		}
		d.sinkErr.Store("")
		// Delete ONLY after a confirmed sink commit. Crash before/during this delete
		// replays the same sequences; the sink high-water makes that replay a no-op.
		d.recordMu.Lock()
		_, err = d.outbox.ExecContext(d.writerCtx, "DELETE FROM events WHERE seq<=?", u.seqs[len(u.seqs)-1])
		if err == nil {
			d.outboxCount -= int64(len(u.events))
			d.outboxBytes -= u.bytes
			d.pending.Store(d.outboxCount)
			d.publishReadinessLocked()
		} else {
			err = d.localFailureLocked(err)
		}
		d.recordMu.Unlock()
		if err != nil {
			return err
		}
		d.written.Add(int64(len(u.events)))
		if !all {
			return nil
		}
	}
}
