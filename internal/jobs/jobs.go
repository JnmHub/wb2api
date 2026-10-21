// Package jobs runs batch operations in the background. Selecting 500 accounts
// and pressing "refresh" must not hold an HTTP request open (the legacy panel
// blocked until every account was done): the request creates a job, returns at
// once, and progress is polled or pushed over the event stream.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// State of a job.
type State string

const (
	Running   State = "running"
	Done      State = "done"
	Cancelled State = "cancelled"
)

// Failure is one item that did not succeed.
type Failure struct {
	Item  string `json:"item"`
	Error string `json:"error"`
}

// Job is a snapshot of a job's progress.
type Job struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	State    State     `json:"state"`
	Total    int       `json:"total"`
	Done     int       `json:"done"`
	Failed   int       `json:"failed"`
	Errors   []Failure `json:"errors,omitempty"` // first maxErrors failures
	Started  int64     `json:"started"`
	Finished int64     `json:"finished,omitempty"`
	// Owner is the replica that runs the job. Other replicas mirror the
	// snapshot and forward cancellation requests through the shared store.
	Owner string `json:"owner,omitempty"`
	// CancelRequested is set by a replica that does not own the job.
	CancelRequested bool `json:"cancel_requested,omitempty"`
}

// Store persists job snapshots so the panel sees the same history from every
// replica. Implementations must keep Save cheap: it is called off the request
// path by a single background writer.
type Store interface {
	Save(ctx context.Context, j Job) error
	Load(ctx context.Context, limit int) ([]Job, error)
	// RequestCancel marks a running job cancelled and reports whether a row
	// matched. The owner observes the flag on its next Sync.
	RequestCancel(ctx context.Context, id string) (bool, error)
}

const maxErrors = 50

// Func processes one item; a non-nil error counts as a failure.
type Func func(ctx context.Context, item string) error

type entry struct {
	mu     sync.Mutex
	job    Job
	cancel context.CancelFunc
	remote bool
}

// Manager keeps recent jobs.
type Manager struct {
	// Workers bounds concurrent items per job (default 4).
	Workers int
	// Keep is how many finished jobs are remembered (default 50).
	Keep int
	// OnProgress is called (throttled) when a job changes; must not block.
	OnProgress func(Job)

	// Store is optional. When set, jobs survive restarts and are mirrored by
	// the other replicas.
	Store Store
	// Owner identifies this process in the shared job table.
	Owner string

	initOnce sync.Once
	saveMu   sync.Mutex
	saveJob  map[string]Job
	saveWake chan struct{}
	syncOnce sync.Once

	mu    sync.Mutex
	jobs  map[string]*entry
	order []string
}

func defaultOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "hub"
	}
	return fmt.Sprintf("%s:%d", host, os.Getpid())
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (m *Manager) ensure() {
	m.initOnce.Do(func() {
		if m.Owner == "" {
			m.Owner = defaultOwner()
		}
		if m.Store != nil {
			m.saveJob = make(map[string]Job)
			m.saveWake = make(chan struct{}, 1)
			go m.saveLoop()
		}
	})
}

func (m *Manager) owner() string {
	if m.Owner != "" {
		return m.Owner
	}
	return defaultOwner()
}

// Start launches a job over items and returns its initial snapshot.
func (m *Manager) Start(kind string, items []string, fn Func) Job {
	m.ensure()
	workers := m.Workers
	if workers <= 0 {
		workers = 4
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &entry{cancel: cancel, job: Job{ID: newID(), Kind: kind, State: Running, Total: len(items), Started: time.Now().Unix(), Owner: m.owner()}}
	m.mu.Lock()
	if m.jobs == nil {
		m.jobs = map[string]*entry{}
	}
	m.jobs[e.job.ID] = e
	m.order = append(m.order, e.job.ID)
	m.pruneLocked()
	m.mu.Unlock()
	snap := e.snapshot()
	m.persist(snap)
	go m.run(ctx, e, items, fn, workers)
	return snap
}

func (e *entry) snapshot() Job {
	e.mu.Lock()
	defer e.mu.Unlock()
	j := e.job
	j.Errors = append([]Failure(nil), e.job.Errors...)
	return j
}

func (m *Manager) run(ctx context.Context, e *entry, items []string, fn Func, workers int) {
	var wg sync.WaitGroup
	ch := make(chan string)
	var lastNotify time.Time
	notify := func(force bool) {
		e.mu.Lock()
		due := force || time.Since(lastNotify) > 250*time.Millisecond
		if due {
			lastNotify = time.Now()
		}
		e.mu.Unlock()
		if !due {
			return
		}
		snap := e.snapshot()
		m.persist(snap)
		if m.OnProgress != nil {
			m.OnProgress(snap)
		}
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range ch {
				err := safeCall(ctx, fn, item)
				e.mu.Lock()
				e.job.Done++
				if err != nil {
					e.job.Failed++
					if len(e.job.Errors) < maxErrors {
						e.job.Errors = append(e.job.Errors, Failure{Item: item, Error: err.Error()})
					}
				}
				e.mu.Unlock()
				notify(false)
			}
		}()
	}
feed:
	for _, it := range items {
		select {
		case ch <- it:
		case <-ctx.Done():
			break feed
		}
	}
	close(ch)
	wg.Wait()
	e.mu.Lock()
	e.job.State = Done
	if ctx.Err() != nil {
		e.job.State = Cancelled
	}
	e.job.Finished = time.Now().Unix()
	e.mu.Unlock()
	notify(true)
}

// persist queues the latest snapshot for the shared store. Saves are
// coalesced per job and written by one goroutine, so a slow database never
// blocks a worker and an older snapshot cannot overwrite a newer one.
func (m *Manager) persist(j Job) {
	if m.Store == nil {
		return
	}
	m.saveMu.Lock()
	if m.saveJob == nil {
		m.saveJob = make(map[string]Job)
	}
	m.saveJob[j.ID] = j
	m.saveMu.Unlock()
	select {
	case m.saveWake <- struct{}{}:
	default:
	}
}

func (m *Manager) saveLoop() {
	for range m.saveWake {
		m.saveMu.Lock()
		batch := m.saveJob
		m.saveJob = make(map[string]Job)
		m.saveMu.Unlock()
		for _, j := range batch {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = m.Store.Save(ctx, j)
			cancel()
		}
	}
}

// StartSync mirrors jobs owned by other replicas and forwards their
// cancellation requests to the owner. It is a no-op without a store.
func (m *Manager) StartSync(ctx context.Context) {
	if m.Store == nil {
		return
	}
	m.ensure()
	m.syncOnce.Do(func() {
		go func() {
			m.Sync(ctx)
			t := time.NewTicker(2 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					m.Sync(ctx)
				}
			}
		}()
	})
}

// Sync loads recent snapshots from the shared store. Jobs owned by this
// process stay authoritative locally; jobs from other replicas are mirrored
// read-only and can be cancelled through RequestCancel.
func (m *Manager) Sync(ctx context.Context) {
	if m.Store == nil {
		return
	}
	m.ensure()
	if ctx == nil {
		ctx = context.Background()
	}
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	limit := m.Keep
	if limit <= 0 {
		limit = 50
	}
	remote, err := m.Store.Load(callCtx, limit)
	if err != nil {
		return
	}
	m.mu.Lock()
	if m.jobs == nil {
		m.jobs = map[string]*entry{}
	}
	for _, j := range remote {
		if j.ID == "" {
			continue
		}
		if j.Owner == m.owner() {
			if j.CancelRequested && j.State == Running {
				if e := m.jobs[j.ID]; e != nil && !e.remote {
					e.cancel()
				}
			}
			continue
		}
		e := m.jobs[j.ID]
		if e == nil {
			e = &entry{job: j, remote: true}
			m.jobs[j.ID] = e
			m.order = append(m.order, j.ID)
			continue
		}
		if e.remote {
			e.mu.Lock()
			e.job = j
			e.mu.Unlock()
		}
	}
	// Remote jobs are appended as they appear, which is not their real age.
	// Keep m.order oldest-first so List stays newest-first.
	sort.SliceStable(m.order, func(i, k int) bool {
		a, b := m.jobs[m.order[i]], m.jobs[m.order[k]]
		if a == nil || b == nil {
			return m.order[i] < m.order[k]
		}
		a.mu.Lock()
		aStarted := a.job.Started
		a.mu.Unlock()
		b.mu.Lock()
		bStarted := b.job.Started
		b.mu.Unlock()
		return aStarted < bStarted
	})
	m.pruneLocked()
	m.mu.Unlock()
}

func safeCall(ctx context.Context, fn Func, item string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &panicError{r}
		}
	}()
	return fn(ctx, item)
}

type panicError struct{ v any }

func (p *panicError) Error() string { return "panic in job item" }

// Get returns a snapshot of the job.
func (m *Manager) Get(id string) (Job, bool) {
	m.mu.Lock()
	e := m.jobs[id]
	m.mu.Unlock()
	if e == nil {
		return Job{}, false
	}
	return e.snapshot(), true
}

// Cancel stops a running job (items already started finish). A job mirrored
// from another replica is cancelled through the shared store; the owner picks
// the request up on its next Sync.
func (m *Manager) Cancel(id string) bool {
	m.mu.Lock()
	e := m.jobs[id]
	m.mu.Unlock()
	if e == nil || e.remote {
		return m.remoteCancel(id)
	}
	e.cancel()
	return true
}

func (m *Manager) remoteCancel(id string) bool {
	if m.Store == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ok, err := m.Store.RequestCancel(ctx, id)
	return err == nil && ok
}

// List returns recent jobs, newest first.
func (m *Manager) List() []Job {
	m.mu.Lock()
	ids := append([]string(nil), m.order...)
	ents := make([]*entry, 0, len(ids))
	for _, id := range ids {
		ents = append(ents, m.jobs[id])
	}
	m.mu.Unlock()
	out := make([]Job, 0, len(ents))
	for i := len(ents) - 1; i >= 0; i-- { // m.order is oldest first
		out = append(out, ents[i].snapshot())
	}
	return out
}

func (m *Manager) pruneLocked() {
	keep := m.Keep
	if keep <= 0 {
		keep = 50
	}
	for len(m.order) > keep {
		oldest := m.order[0]
		e := m.jobs[oldest]
		e.mu.Lock()
		running := e.job.State == Running
		e.mu.Unlock()
		if running {
			break
		}
		delete(m.jobs, oldest)
		m.order = m.order[1:]
	}
}
