package quota

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"sync"
	"time"

	"wb2api/internal/pool"
)

// Window-probe constants (legacy wb_window_probe.py).
const (
	ProbePrompt     = "Hi"
	ProbeMaxTokens  = 16
	ProbeTimeout    = 60 * time.Second
	MaxAttempts     = 20
	RetryDelay      = 15 * time.Second
	FreshWindowMin  = 30 * time.Second
	MaxWindowAge    = 7 * 24 * time.Hour
	BatchLimit      = 8
	ProbeGap        = 400 * time.Millisecond
	windowsFileName = "model_windows.json"
)

// Window is one registered quota window: the instant the upstream said the
// line resets, plus the probe's own retry state.
type Window struct {
	UID        string  `json:"uid"`
	Model      string  `json:"model"`
	ResetAt    float64 `json:"reset_at"`
	Attempts   int     `json:"attempts"`
	LastTry    float64 `json:"last_try"`
	LastResult string  `json:"last_result"`
}

// Prober sends the minimal probe request and reports what the line did.
type Prober interface {
	ProbeModel(ctx context.Context, acc *pool.Account, model string) (ok bool, reset time.Time, detail string)
}

// Windows is the persisted set of quota windows plus the probe runner.
type Windows struct {
	Dir    string
	Now    func() time.Time
	Gate   func() bool
	Prober Prober
	Log    *slog.Logger

	mu     sync.Mutex
	rows   map[string]Window
	loaded bool
}

func (w *Windows) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Windows) enabled() bool { return w.Gate == nil || w.Gate() }

func (w *Windows) path() string { return w.Dir + "/" + windowsFileName }

func key(uid, model string) string { return uid + "\x00" + model }

func (w *Windows) load() {
	if w.loaded {
		return
	}
	w.loaded = true
	w.rows = map[string]Window{}
	if w.Dir == "" {
		return
	}
	var doc struct {
		Version int      `json:"version"`
		Windows []Window `json:"windows"`
	}
	if readJSONFile(w.path(), &doc) && len(doc.Windows) > 0 {
		w.addPruned(doc.Windows)
		return
	}
	var bare []Window
	if readJSONFile(w.path(), &bare) {
		w.addPruned(bare)
	}
}

func (w *Windows) addPruned(list []Window) {
	now := w.now()
	cut := now.Add(-MaxWindowAge).Unix()
	for _, r := range list {
		if r.UID == "" || r.Model == "" || r.ResetAt <= 0 || int64(r.ResetAt) < cut {
			continue
		}
		r.LastResult = clamp(r.LastResult, 200)
		w.rows[key(r.UID, r.Model)] = r
	}
}

func (w *Windows) saveLocked() {
	if w.Dir == "" {
		return
	}
	rows := make([]Window, 0, len(w.rows))
	for _, r := range w.rows {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ResetAt < rows[j].ResetAt })
	doc := map[string]any{"version": 1, "saved_at": stamp(w.now()), "windows": rows}
	if err := writeJSONAtomic(w.path(), doc); err != nil && w.Log != nil {
		w.Log.Warn("window probe: cannot save state", "err", err)
	}
}

// Schedule registers (or refreshes) a window. It returns true when the state
// changed; a repeated 429 for the same reset instant is a no-op.
func (w *Windows) Schedule(uid, model string, reset time.Time) bool {
	if uid == "" || model == "" || !w.enabled() {
		return false
	}
	now := w.now()
	if !reset.After(now) {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.load()
	if old, ok := w.rows[key(uid, model)]; ok && absDiff(old.ResetAt, reset) <= 1.0 {
		return false
	}
	w.rows[key(uid, model)] = Window{UID: uid, Model: model, ResetAt: float64(reset.UnixNano()) / 1e9, LastTry: float64(now.UnixNano()) / 1e9}
	w.saveLocked()
	return true
}

func absDiff(a float64, b time.Time) float64 {
	d := a - float64(b.UnixNano())/1e9
	if d < 0 {
		return -d
	}
	return d
}

// Has reports whether the pair currently owns a window record.
func (w *Windows) Has(uid, model string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.load()
	_, ok := w.rows[key(uid, model)]
	return ok
}

// Snapshot lists the windows, soonest reset first.
func (w *Windows) Snapshot() []Window {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.load()
	out := make([]Window, 0, len(w.rows))
	for _, r := range w.rows {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ResetAt < out[j].ResetAt })
	return out
}

// SecondsUntilDue is the wait until the next window opens (ok=false when none).
func (w *Windows) SecondsUntilDue() (time.Duration, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.load()
	if len(w.rows) == 0 {
		return 0, false
	}
	soonest := time.Time{}
	for _, r := range w.rows {
		t := time.Unix(0, int64(r.ResetAt*1e9))
		if soonest.IsZero() || t.Before(soonest) {
			soonest = t
		}
	}
	d := soonest.Sub(w.now())
	if d < 0 {
		d = 0
	}
	return d, true
}

// RunDue probes every window whose reset instant has passed, at most limit.
// It returns how many probes were actually sent.
func (w *Windows) RunDue(ctx context.Context, p *pool.Pool, limit int) int {
	if !w.enabled() || w.Prober == nil {
		return 0
	}
	if limit <= 0 {
		limit = BatchLimit
	}
	now := w.now()
	type due struct {
		uid, model string
		reset      float64
	}
	var list []due
	w.mu.Lock()
	w.load()
	for _, r := range w.rows {
		if r.ResetAt <= float64(now.UnixNano())/1e9 {
			list = append(list, due{r.UID, r.Model, r.ResetAt})
		}
	}
	w.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].reset < list[j].reset })
	if len(list) > limit {
		list = list[:limit]
	}
	sent := 0
	for i, d := range list {
		if ctx.Err() != nil {
			break
		}
		if i > 0 {
			select {
			case <-time.After(ProbeGap):
			case <-ctx.Done():
				return sent
			}
		}
		acc := p.Get(d.uid)
		if acc == nil {
			w.mu.Lock()
			w.load()
			delete(w.rows, key(d.uid, d.model))
			w.saveLocked()
			w.mu.Unlock()
			w.logf("window probe: account %s is no longer in the pool, dropping %s", short(d.uid), d.model)
			continue
		}
		rec := acc.Record()
		if !rec.IsEnabled() || rec.AccessToken == "" {
			continue
		}
		sent++
		ok, reset, detail := w.Prober.ProbeModel(ctx, acc, d.model)
		w.mu.Lock()
		w.load()
		row, exists := w.rows[key(d.uid, d.model)]
		if exists {
			switch {
			case ok:
				acc.ClearModelCooldown(d.model)
				delete(w.rows, key(d.uid, d.model))
				w.logf("window probe: account %s model %s reopened", short(d.uid), d.model)
			case !reset.IsZero() && reset.After(now.Add(FreshWindowMin)):
				row.ResetAt = float64(reset.UnixNano()) / 1e9
				row.Attempts = 0
				row.LastTry = float64(now.UnixNano()) / 1e9
				row.LastResult = clamp(detail, 200)
				w.rows[key(d.uid, d.model)] = row
				w.logf("window probe: account %s model %s still limited, next reset %s", short(d.uid), d.model, reset.Format(time.RFC3339))
			default:
				row.Attempts++
				if row.Attempts >= MaxAttempts {
					delete(w.rows, key(d.uid, d.model))
					w.logf("window probe: account %s model %s did not reopen after %d attempts, giving up", short(d.uid), d.model, MaxAttempts)
				} else {
					row.ResetAt = float64(now.Add(RetryDelay).UnixNano()) / 1e9
					row.LastTry = float64(now.UnixNano()) / 1e9
					row.LastResult = clamp(detail, 200)
					w.rows[key(d.uid, d.model)] = row
					w.logf("window probe: account %s model %s not reset yet (%s), retrying in %s", short(d.uid), d.model, clamp(detail, 80), RetryDelay)
				}
			}
			w.saveLocked()
		}
		w.mu.Unlock()
	}
	return sent
}

func (w *Windows) logf(format string, a ...any) {
	if w.Log != nil {
		w.Log.Info("quota: "+format, a...)
	}
}

func short(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// ResetJSON encodes the windows for the panel (read-only).
func (w *Windows) ResetJSON() json.RawMessage {
	b, _ := json.Marshal(w.Snapshot())
	return b
}
