// Package usageest estimates how much of each account's 24-hour model quota is
// left. The upstream never publishes the budget, so it is inferred from the
// usage samples taken at the moment a window 429 arrived (legacy wb_proxy.py
// remaining-usage estimate). The estimate is display-only except for the
// opt-in "remaining priority" dispatch preference.
package usageest

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/usagedb"
)

const (
	windowSeconds = 24 * 3600.0
	keepSeconds   = 26 * 3600.0
	maxEvents     = 1000
	eventsName    = "limit-events.jsonl"

	// Weights of the three estimate days (today, yesterday, day before).
	wToday, wYesterday, wBefore = 6.0, 3.0, 1.0

	// Dispatch bands: remaining ratio <= ceiling gets the weight.
	bandVeryLow = 0.05
	bandLow     = 0.15
	bandMedium  = 0.30
)

// Row is one (account, model) estimate.
type Row struct {
	UID       string   `json:"uid"`
	Nickname  string   `json:"nickname"`
	Realm     string   `json:"realm"`
	Model     string   `json:"model"`
	Used      int64    `json:"used"`
	Budget    *int64   `json:"budget"`
	Remaining *int64   `json:"remaining"`
	UsedPct   *float64 `json:"used_pct"`
	ResetAt   float64  `json:"reset_at,omitempty"`
	Cooling   bool     `json:"cooling"`
	Estimated bool     `json:"estimated"`
}

// ModelTotal aggregates the rows of one (realm, model).
type ModelTotal struct {
	Realm          string `json:"realm"`
	Model          string `json:"model"`
	Budget         *int64 `json:"budget"`
	TotalRemaining *int64 `json:"total_remaining"`
	Pairs          int    `json:"pairs"`
	Cooling        int    `json:"cooling"`
	Unknown        int    `json:"unknown"`
}

// Payload is what the panel shows.
type Payload struct {
	WindowSeconds int          `json:"window_seconds"`
	GeneratedAt   int64        `json:"generated_at"`
	Rows          []Row        `json:"rows"`
	Models        []ModelTotal `json:"models"`
	ETag          string       `json:"etag"`
}

// Estimator owns the limit-event file and the cached estimate.
type Estimator struct {
	DB   *usagedb.DB
	Pool *pool.Pool
	Dir  string
	Now  func() time.Time
	TTL  time.Duration
	Log  *slog.Logger

	mu      sync.Mutex
	cached  *Payload
	builtAt time.Time

	evMu sync.Mutex
}

func (e *Estimator) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Estimator) ttl() time.Duration {
	if e.TTL > 0 {
		return e.TTL
	}
	if v := strings.TrimSpace(os.Getenv("WB_REMAINING_TTL")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Second
		}
	}
	return 15 * time.Second
}

func (e *Estimator) logger() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

// ---------------------------------------------------------------- limit events

type limitEvent struct {
	At      float64 `json:"at"`
	Account string  `json:"account"`
	Model   string  `json:"model"`
	Realm   string  `json:"realm"`
	Reset   float64 `json:"reset"`
}

func (e *Estimator) eventsPath() string { return filepath.Join(e.Dir, eventsName) }

// NoteLimit records a model-scoped 429 that carried a reset clock. It is a
// best-effort append: an unwritable file must never affect the request.
func (e *Estimator) NoteLimit(uid, model, realmName string, reset, at time.Time) {
	if uid == "" || model == "" || reset.IsZero() {
		return
	}
	ev := limitEvent{At: float64(at.UnixNano()) / 1e9, Account: uid, Model: model, Realm: realmName,
		Reset: float64(reset.UnixNano()) / 1e9}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	e.evMu.Lock()
	defer e.evMu.Unlock()
	if e.Dir == "" {
		return
	}
	if err := os.MkdirAll(e.Dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(e.eventsPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		e.logger().Warn("remaining estimate: cannot append a limit event", "err", err)
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
	e.mu.Lock()
	e.cached = nil
	e.mu.Unlock()
}

// loadEvents returns the events, newest last, trimmed to maxEvents.
func (e *Estimator) loadEvents() []limitEvent {
	if e.Dir == "" {
		return nil
	}
	f, err := os.Open(e.eventsPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []limitEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev limitEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Account == "" || ev.Model == "" || ev.At <= 0 {
			continue
		}
		out = append(out, ev)
	}
	if len(out) > maxEvents {
		out = out[len(out)-maxEvents:]
	}
	return out
}

// ------------------------------------------------------------------ estimate

type series struct {
	at  []float64
	cum []int64
}

func (s *series) add(at float64, total int64) {
	n := len(s.at)
	s.at = append(s.at, at)
	if n == 0 {
		s.cum = append(s.cum, total)
		return
	}
	s.cum = append(s.cum, s.cum[n-1]+total)
}

func (s *series) lastAt() float64 {
	if len(s.at) == 0 {
		return 0
	}
	return s.at[len(s.at)-1]
}

// sum returns the tokens in [from, to).
func (s *series) sum(from, to float64) int64 {
	if len(s.at) == 0 || to <= from {
		return 0
	}
	i := sort.SearchFloat64s(s.at, from)
	j := sort.SearchFloat64s(s.at, to)
	if i >= len(s.at) || j <= i {
		return 0
	}
	before := int64(0)
	if i > 0 {
		before = s.cum[i-1]
	}
	end := s.cum[len(s.cum)-1]
	if j < len(s.at) {
		end = s.cum[j-1]
	}
	return end - before
}

func pairKey(uid, model string) string { return uid + "\x00" + model }

type budget struct {
	realm, model string
	avg          int64
}

type builder struct {
	est     *Estimator
	now     time.Time
	series  map[string]*series
	resets  map[string][]float64 // pair -> reset instants, ascending
	events  []limitEvent
	budgets map[string]budget // realm\x00model
}

func (e *Estimator) snapshot() *Payload {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	if e.cached != nil && now.Sub(e.builtAt) < e.ttl() {
		return e.cached
	}
	b := &builder{est: e, now: now, series: map[string]*series{}, resets: map[string][]float64{}, budgets: map[string]budget{}}
	b.loadPoints()
	b.loadEvents()
	b.estimateBudgets()
	p := b.rows()
	p.WindowSeconds = int(windowSeconds)
	p.GeneratedAt = now.Unix()
	p.ETag = sum(p)
	e.cached, e.builtAt = p, now
	return p
}

func sum(p *Payload) string {
	b, _ := json.Marshal(struct {
		Rows []Row `json:"rows"`
	}{p.Rows})
	h := sha1.Sum(b)
	return hex.EncodeToString(h[:8])
}

func (b *builder) loadPoints() {
	if b.est.DB == nil {
		return
	}
	from := b.now.Add(-time.Duration(keepSeconds) * time.Second)
	points, err := b.est.DB.PairPoints(from, b.now)
	if err != nil {
		b.est.logger().Warn("remaining estimate: cannot read usage", "err", err)
		return
	}
	for _, p := range points {
		k := pairKey(p.Account, p.Model)
		s := b.series[k]
		if s == nil {
			s = &series{}
			b.series[k] = s
		}
		s.add(p.At, p.Total)
	}
}

func (b *builder) loadEvents() {
	b.events = b.est.loadEvents()
	for _, ev := range b.events {
		if ev.Reset <= 0 {
			continue
		}
		k := pairKey(ev.Account, ev.Model)
		b.resets[k] = append(b.resets[k], ev.Reset)
	}
	for k := range b.resets {
		sort.Float64s(b.resets[k])
	}
}

// capStart is the start of the window that ends at reset: the previous reset
// (chained windows cut the old one out) or 24h before, whichever is later.
func (b *builder) capStart(k string, reset float64) float64 {
	start := reset - windowSeconds
	for _, r := range b.resets[k] {
		if r < reset && r > start {
			start = r
		}
	}
	return start
}

func (b *builder) estimateBudgets() {
	type dayAccount struct {
		sum int64
		n   int64
	}
	samples := map[string]map[string]map[string]*dayAccount{} // modelKey -> day -> account -> totals
	for _, ev := range b.events {
		if ev.Reset <= 0 {
			continue
		}
		k := pairKey(ev.Account, ev.Model)
		s := b.series[k]
		if s == nil {
			continue
		}
		sample := s.sum(b.capStart(k, ev.Reset), ev.At)
		if sample <= 0 {
			continue
		}
		mk := ev.Realm + "\x00" + ev.Model
		day := time.Unix(0, int64(ev.At*1e9)).In(time.Local).Format("2006-01-02")
		if samples[mk] == nil {
			samples[mk] = map[string]map[string]*dayAccount{}
		}
		if samples[mk][day] == nil {
			samples[mk][day] = map[string]*dayAccount{}
		}
		d := samples[mk][day][ev.Account]
		if d == nil {
			d = &dayAccount{}
			samples[mk][day][ev.Account] = d
		}
		d.sum += sample
		d.n++
	}
	today := b.now.In(time.Local).Format("2006-01-02")
	yesterday := b.now.AddDate(0, 0, -1).In(time.Local).Format("2006-01-02")
	before := b.now.AddDate(0, 0, -2).In(time.Local).Format("2006-01-02")
	for mk, byDay := range samples {
		var sumW, sumAvg float64
		for day, byAcc := range byDay {
			w := 0.0
			switch day {
			case today:
				w = wToday
			case yesterday:
				w = wYesterday
			case before:
				w = wBefore
			default:
				continue
			}
			var accSum float64
			for _, d := range byAcc {
				accSum += float64(d.sum) / float64(d.n)
			}
			dayAvg := accSum / float64(len(byAcc))
			sumW += w
			sumAvg += w * dayAvg
		}
		if sumW == 0 {
			continue
		}
		parts := strings.SplitN(mk, "\x00", 2)
		b.budgets[mk] = budget{realm: parts[0], model: parts[1], avg: int64(math.Round(sumAvg / sumW))}
	}
}

func (b *builder) rows() *Payload {
	p := &Payload{Rows: []Row{}, Models: []ModelTotal{}}
	if b.est.Pool == nil {
		return p
	}
	byModel := map[string]*ModelTotal{}
	for _, acc := range b.est.Pool.All() {
		rec := acc.Record()
		if !rec.IsEnabled() || rec.AccessToken == "" || rec.UID == "" {
			continue
		}
		models := map[string]bool{}
		for k, s := range b.series {
			if !strings.HasPrefix(k, rec.UID+"\x00") {
				continue
			}
			if s.lastAt() >= float64(b.now.Add(-24*time.Hour).UnixNano())/1e9 {
				models[strings.TrimPrefix(k, rec.UID+"\x00")] = true
			}
		}
		for model := range rec.ModelCaps {
			models[model] = true
		}
		var names []string
		for m := range models {
			names = append(names, m)
		}
		sort.Strings(names)
		for _, model := range names {
			k := pairKey(rec.UID, model)
			s := b.series[k]
			if s == nil {
				s = &series{}
			}
			bud, hasBudget := b.budgets[rec.Realm+"\x00"+model]
			row := Row{UID: rec.UID, Nickname: rec.Nickname, Realm: rec.Realm, Model: model}
			upcoming, past := 0.0, 0.0
			for _, r := range b.resets[k] {
				if r > float64(b.now.UnixNano())/1e9 {
					if upcoming == 0 || r < upcoming {
						upcoming = r
					}
				} else if r > past {
					past = r
				}
			}
			if c, ok := rec.ModelCaps[model]; ok && c.Reset > 0 {
				if c.Reset > float64(b.now.UnixNano())/1e9 {
					if upcoming == 0 || c.Reset < upcoming {
						upcoming = c.Reset
					}
				} else if c.Reset > past {
					past = c.Reset
				}
			}
			nowF := float64(b.now.UnixNano()) / 1e9
			switch {
			case upcoming > 0:
				row.Cooling = true
				row.ResetAt = upcoming
				row.Used = s.sum(b.capStart(k, upcoming), nowF)
				zero := int64(0)
				row.Remaining = &zero
			case past > 0:
				row.ResetAt = past
				row.Used = s.sum(math.Max(past, nowF-windowSeconds), nowF)
				row.Estimated = past < nowF-windowSeconds
				if hasBudget {
					v := bud.avg - row.Used
					if v < 0 {
						v = 0
					}
					row.Remaining = &v
				}
			default:
				row.Estimated = true
				row.Used = s.sum(nowF-windowSeconds, nowF)
				if hasBudget {
					v := bud.avg - row.Used
					if v < 0 {
						v = 0
					}
					row.Remaining = &v
				}
			}
			if hasBudget {
				bv := bud.avg
				row.Budget = &bv
				if bv > 0 {
					pct := float64(row.Used) * 100 / float64(bv)
					row.UsedPct = &pct
				}
			}
			if row.Used == 0 && !row.Cooling && !hasBudget && len(rec.ModelCaps) == 0 {
				continue
			}
			p.Rows = append(p.Rows, row)
			mk := rec.Realm + "\x00" + model
			mt := byModel[mk]
			if mt == nil {
				mt = &ModelTotal{Realm: rec.Realm, Model: model}
				byModel[mk] = mt
			}
			mt.Pairs++
			if row.Cooling {
				mt.Cooling++
			}
			if row.Remaining == nil {
				mt.Unknown++
			} else {
				v := *row.Remaining
				if mt.TotalRemaining == nil {
					mt.TotalRemaining = &v
				} else {
					x := *mt.TotalRemaining + v
					mt.TotalRemaining = &x
				}
			}
			if hasBudget && mt.Budget == nil {
				bv := bud.avg
				mt.Budget = &bv
			}
		}
	}
	sort.SliceStable(p.Rows, func(i, j int) bool {
		a, b := p.Rows[i], p.Rows[j]
		if (a.Remaining == nil) != (b.Remaining == nil) {
			return a.Remaining != nil
		}
		av, bv := int64(0), int64(0)
		if a.Remaining != nil {
			av = *a.Remaining
		}
		if b.Remaining != nil {
			bv = *b.Remaining
		}
		return av < bv
	})
	for _, mt := range byModel {
		p.Models = append(p.Models, *mt)
	}
	sort.SliceStable(p.Models, func(i, j int) bool {
		a, b := p.Models[i], p.Models[j]
		if (a.TotalRemaining == nil) != (b.TotalRemaining == nil) {
			return a.TotalRemaining != nil
		}
		av, bv := int64(0), int64(0)
		if a.TotalRemaining != nil {
			av = *a.TotalRemaining
		}
		if b.TotalRemaining != nil {
			bv = *b.TotalRemaining
		}
		if av != bv {
			return av < bv
		}
		return a.Model < b.Model
	})
	return p
}

// Payload returns the cached estimate.
func (e *Estimator) Payload() *Payload { return e.snapshot() }

// ScheduleWeights turns the estimate into the dispatch preference: a pair with
// little of its budget left gets more traffic, so the quota is spent before it
// resets. Pairs without a budget, zero remaining or cooling are left out.
func (e *Estimator) ScheduleWeights() map[string]map[string]int {
	p := e.snapshot()
	out := map[string]map[string]int{}
	for _, r := range p.Rows {
		if r.Cooling || r.Budget == nil || r.Remaining == nil || *r.Budget <= 0 || *r.Remaining <= 0 {
			continue
		}
		ratio := float64(*r.Remaining) / float64(*r.Budget)
		w := 1
		switch {
		case ratio <= bandVeryLow:
			w = 4
		case ratio <= bandLow:
			w = 3
		case ratio <= bandMedium:
			w = 2
		}
		if w <= 1 {
			continue
		}
		if out[r.Model] == nil {
			out[r.Model] = map[string]int{}
		}
		out[r.Model][r.UID] = w
	}
	return out
}

func (e *Estimator) String() string {
	p := e.snapshot()
	return fmt.Sprintf("remaining estimate: %d rows, %d models", len(p.Rows), len(p.Models))
}
