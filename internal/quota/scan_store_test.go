package quota

import (
	"context"
	"sync"
	"testing"
	"time"
)

type memScanStore struct {
	mu   sync.Mutex
	rows []ScanRow
}

func (m *memScanStore) LoadScans() ([]ScanRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ScanRow, len(m.rows))
	copy(out, m.rows)
	return out, nil
}

func (m *memScanStore) SaveScans(rows []ScanRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = make([]ScanRow, len(rows))
	copy(m.rows, rows)
	return nil
}

func TestScanStoreSharesScheduleAcrossReplicas(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	store := &memScanStore{}
	models := func() []string { return []string{"m1"} }
	p := scanPool(t, "u1")

	a := &Scanner{Dir: t.TempDir(), Now: func() time.Time { return now }, Prober: &fakeScanProber{ok: true},
		Models: models, Store: store}
	if sent := a.RunDue(context.Background(), p, true); sent != 1 {
		t.Fatalf("first replica sent %d probes, want 1", sent)
	}

	bProber := &fakeScanProber{ok: true}
	b := &Scanner{Dir: t.TempDir(), Now: func() time.Time { return now }, Prober: bProber,
		Models: models, Store: store}
	rows := b.Snapshot()
	if len(rows) != 1 || rows[0].UID != "u1" || rows[0].LastResult != "ok" {
		t.Fatalf("second replica did not see the shared scan row: %+v", rows)
	}
	// The shared LastScan is recent, so a takeover must wait a full interval
	// instead of probing the same line again.
	if sent := b.RunDue(context.Background(), p, false); sent != 0 || bProber.calls != 0 {
		t.Fatalf("second replica re-probed a fresh line: sent=%d calls=%d", sent, bProber.calls)
	}
}
