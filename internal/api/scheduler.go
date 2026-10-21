package api

import (
	"math"
	"net/http"
	"sort"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/sched"
)

// Scheduler exposes a read-only view of the live scheduler. The panel uses the
// aggregate numbers for its tiles; the per-account list is capped so a large
// pool does not turn one poll into a multi-megabyte response.
type Scheduler struct {
	Pool     *pool.Pool
	Feedback *sched.Stats
	TopK     func() int
	Score    func(*pool.Account) int64
	Realm    func() string
	// MaxInflight is the shared per-account slot limit (Redis), used when an
	// account has no explicit local cap. Without it the panel reports zero
	// capacity even though the shared limiter is enforcing a bound.
	MaxInflight int
}

func (s *Scheduler) limit(a *pool.Account) int {
	if max := a.MaxInflight(); max > 0 {
		return max
	}
	return s.MaxInflight
}

func (s *Scheduler) Register(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mux.Handle("GET /api/v2/scheduler/summary", wrap(http.HandlerFunc(s.summary)))
}

type schedulerAccount struct {
	UID         string  `json:"uid"`
	ShortID     string  `json:"short_id"`
	Nickname    string  `json:"nickname"`
	Realm       string  `json:"realm"`
	State       string  `json:"state"`
	Inflight    int     `json:"inflight"`
	MaxInflight int     `json:"max_inflight"`
	Weight      int     `json:"weight"`
	Score       int64   `json:"score"`
	TTFTMS      float64 `json:"ttft_ms,omitempty"`
	ErrorRate   float64 `json:"error_rate,omitempty"`
}

func (s *Scheduler) summary(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	all := s.Pool.All()
	states := map[string]int{}
	var inflight, capacity, tracked int
	var sumTTFT, sumErr float64
	rows := make([]schedulerAccount, 0, len(all))
	for _, a := range all {
		state := string(a.State(now))
		states[state]++
		inflight += a.Inflight()
		limit := s.limit(a)
		capacity += limit
		var ttft, errRate float64
		if s.Feedback != nil {
			if v, e, ok := s.Feedback.Score(a.UID); ok {
				ttft, errRate = v, e
				tracked++
				sumTTFT += v
				sumErr += e
			}
		}
		var score int64
		if s.Score != nil {
			score = s.Score(a)
		}
		rows = append(rows, schedulerAccount{
			UID: a.UID, ShortID: shortID(a.UID), Nickname: a.Record().Nickname, Realm: a.Realm,
			State: state, Inflight: a.Inflight(), MaxInflight: limit, Weight: a.Weight(),
			Score: score, TTFTMS: round1(ttft), ErrorRate: round1(errRate * 100),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Score != rows[j].Score {
			return rows[i].Score > rows[j].Score
		}
		return rows[i].UID < rows[j].UID
	})
	if len(rows) > 20 {
		rows = rows[:20]
	}
	avgTTFT, avgErr := 0.0, 0.0
	if tracked > 0 {
		avgTTFT = round1(sumTTFT / float64(tracked))
		avgErr = round1(sumErr / float64(tracked) * 100)
	}
	realm := ""
	if s.Realm != nil {
		realm = s.Realm()
	}
	topK := 0
	if s.TopK != nil {
		topK = s.TopK()
	}
	affinity := s.Pool.Affinity().SyncStats()
	writeJSON(w, http.StatusOK, map[string]any{
		"realm": realm, "top_k": topK,
		"total": len(all), "inflight": inflight, "capacity": capacity,
		"tracked": tracked, "ttft_ms": avgTTFT, "error_rate": avgErr,
		"states": states, "hot": rows,
		"affinity": map[string]any{
			"shared": affinity.Shared, "pending": affinity.Pending,
			"dropped": affinity.Dropped, "errors": affinity.Errors,
		},
	})
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}
