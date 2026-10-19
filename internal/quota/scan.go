package quota

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"wb2api/internal/pool"
)

// Active quota scan: probes the watched models every few hours so a fully used
// line is discovered before a real request hits it. Port of wb_quota_scan.py.
// Once a line has a registered 429 window it is left to the window probe.
const (
	scanInterval   = 6 * time.Hour
	scanRetryDelay = 15 * time.Minute
	scanMaxAttempt = 6
	scanFirstDelay = 5 * time.Second
	scanStagger    = 15 * time.Second
	scanFirstMax   = time.Hour
	scanBatch      = 6
	scanGap        = 400 * time.Millisecond
	scanMaxAge     = 30 * 24 * time.Hour
	scanFileName   = "quota_scan.json"
)

// ScanRow is one watched (account, model) pair.
type ScanRow struct {
	UID        string  `json:"uid"`
	Model      string  `json:"model"`
	LastScan   float64 `json:"last_scan"`
	NextAt     float64 `json:"next_at"`
	SeenAt     float64 `json:"seen_at"`
	Attempts   int     `json:"attempts"`
	LastTry    float64 `json:"last_try"`
	LastResult string  `json:"last_result"`
	LastDetail string  `json:"last_detail"`
}

// Scanner runs the active quota probes.
type Scanner struct {
	Dir     string
	Now     func() time.Time
	Prober  Prober
	Windows *Windows
	// Models returns the watched model ids (settings.quota_scan_models).
	Models func() []string
	Log    *slog.Logger

	mu     sync.Mutex
	rows   map[string]ScanRow
	loaded bool
}

func (s *Scanner) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scanner) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Scanner) path() string { return s.Dir + "/" + scanFileName }

func (s *Scanner) load() {
	if s.loaded {
		return
	}
	s.loaded = true
	s.rows = map[string]ScanRow{}
	if s.Dir == "" {
		return
	}
	var doc struct {
		Scans []ScanRow `json:"scans"`
	}
	if readJSONFile(s.path(), &doc) {
		cut := s.now().Add(-scanMaxAge).Unix()
		for _, r := range doc.Scans {
			if r.UID == "" || r.Model == "" {
				continue
			}
			if int64(r.LastTry) < cut && int64(r.SeenAt) < cut {
				continue
			}
			r.LastDetail = clamp(r.LastDetail, 200)
			r.LastResult = clamp(r.LastResult, 40)
			s.rows[key(r.UID, r.Model)] = r
		}
	}
}

func (s *Scanner) saveLocked() {
	if s.Dir == "" {
		return
	}
	rows := make([]ScanRow, 0, len(s.rows))
	for _, r := range s.rows {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].UID != rows[j].UID {
			return rows[i].UID < rows[j].UID
		}
		return rows[i].Model < rows[j].Model
	})
	doc := map[string]any{"version": 1, "saved_at": stamp(s.now()), "interval_hours": 6, "scans": rows}
	if err := writeJSONAtomic(s.path(), doc); err != nil {
		s.logger().Warn("quota scan: cannot save state", "err", err)
	}
}

// Snapshot lists the watch list (for the panel).
func (s *Scanner) Snapshot() []ScanRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	out := make([]ScanRow, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].UID < out[j].UID
	})
	return out
}

// seed creates rows for newly watched (account, model) pairs; existing rows are
// untouched so their schedules survive restarts.
func (s *Scanner) seed(p *pool.Pool) {
	models := s.Models()
	if len(models) == 0 || p == nil {
		return
	}
	now := s.now()
	delay := scanFirstDelay
	seen := map[string]bool{}
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		for _, acc := range p.All() {
			rec := acc.Record()
			if !rec.IsEnabled() || rec.AccessToken == "" {
				continue
			}
			k := key(rec.UID, model)
			if _, ok := s.rows[k]; ok {
				continue
			}
			s.rows[k] = ScanRow{UID: rec.UID, Model: model, SeenAt: float64(now.UnixNano()) / 1e9,
				NextAt: float64(now.Add(delay).UnixNano()) / 1e9}
			seen[k] = true
			delay += scanStagger
			if delay > scanFirstMax {
				delay = scanFirstMax
			}
		}
	}
	if len(seen) > 0 {
		s.saveLocked()
	}
}

// RunDue probes the pairs whose next check is due, at most scanBatch. force
// bypasses only the time window, never the watched model / window / cooling
// filters. It returns how many probes were sent.
func (s *Scanner) RunDue(ctx context.Context, p *pool.Pool, force bool) int {
	if len(s.Models()) == 0 || s.Prober == nil || p == nil {
		return 0
	}
	now := s.now()
	type target struct {
		k   string
		row ScanRow
		due float64
		acc *pool.Account
	}
	var list []target
	s.mu.Lock()
	s.load()
	s.seed(p)
	nowF := float64(now.UnixNano()) / 1e9
	for k, r := range s.rows {
		acc := p.Get(r.UID)
		if acc == nil {
			continue // keep the row: the account may come back
		}
		if s.Windows != nil && s.Windows.Has(r.UID, r.Model) {
			continue // the 429 window owns this line now
		}
		if acc.ThrottleWait(r.Model, now) > 0 {
			continue
		}
		due := r.NextAt
		if r.LastScan > 0 {
			if t := float64(time.Unix(0, int64(r.LastScan*1e9)).Add(scanInterval).UnixNano()) / 1e9; t > due {
				due = t
			}
		}
		if !force && nowF < due {
			continue
		}
		list = append(list, target{k: k, row: r, due: due, acc: acc})
	}
	s.mu.Unlock()
	sort.Slice(list, func(i, j int) bool {
		if list[i].due != list[j].due {
			return list[i].due < list[j].due
		}
		return list[i].row.Model < list[j].row.Model
	})
	if len(list) > scanBatch {
		list = list[:scanBatch]
	}
	sent := 0
	for i, t := range list {
		if ctx.Err() != nil {
			break
		}
		if i > 0 {
			select {
			case <-time.After(scanGap):
			case <-ctx.Done():
				return sent
			}
		}
		sent++
		ok, reset, detail := s.Prober.ProbeModel(ctx, t.acc, t.row.Model)
		s.mu.Lock()
		s.load()
		row, exists := s.rows[t.k]
		if !exists {
			s.mu.Unlock()
			continue
		}
		switch {
		case ok:
			t.acc.ClearModelCooldown(t.row.Model)
			row.LastScan = nowF
			row.LastResult = "ok"
			row.LastDetail = ""
			s.logger().Info("quota scan: line is open", "account", short(t.row.UID), "model", t.row.Model)
		case !reset.IsZero() && reset.After(now.Add(5*time.Second)):
			wait := reset.Sub(now)
			if wait < time.Second {
				wait = time.Second
			}
			t.acc.NoteModelError(t.row.Model, reset, wait, "HTTP 429 (model throttled)", detail, now)
			if s.Windows != nil {
				s.Windows.Schedule(t.row.UID, t.row.Model, reset)
			}
			row.LastScan = nowF
			row.Attempts = 0
			row.LastResult = "throttled"
			row.LastDetail = clamp(detail, 200)
			s.logger().Info("quota scan: line is capped, handing it to the window probe",
				"account", short(t.row.UID), "model", t.row.Model, "reset", reset.Format(time.RFC3339))
		default:
			row.Attempts++
			row.LastTry = nowF
			row.LastDetail = clamp(detail, 200)
			if strings.Contains(detail, "429") {
				row.LastResult = "throttled"
			} else {
				row.LastResult = "error"
			}
			if row.Attempts >= scanMaxAttempt {
				row.LastScan = nowF
				row.Attempts = 0
				row.NextAt = 0
				s.logger().Info("quota scan: giving up for one full interval", "account", short(t.row.UID), "model", t.row.Model)
			} else {
				row.NextAt = float64(now.Add(scanRetryDelay).UnixNano()) / 1e9
			}
		}
		s.rows[t.k] = row
		s.saveLocked()
		s.mu.Unlock()
	}
	return sent
}
