// Package jobs runs batch operations in the background. Selecting 500 accounts
// and pressing "refresh" must not hold an HTTP request open (the legacy panel
// blocked until every account was done): the request creates a job, returns at
// once, and progress is polled or pushed over the event stream.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
}

const maxErrors = 50

// Func processes one item; a non-nil error counts as a failure.
type Func func(ctx context.Context, item string) error

type entry struct {
	mu     sync.Mutex
	job    Job
	cancel context.CancelFunc
}

// Manager keeps recent jobs.
type Manager struct {
	// Workers bounds concurrent items per job (default 4).
	Workers int
	// Keep is how many finished jobs are remembered (default 50).
	Keep int
	// OnProgress is called (throttled) when a job changes; must not block.
	OnProgress func(Job)

	mu    sync.Mutex
	jobs  map[string]*entry
	order []string
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Start launches a job over items and returns its initial snapshot.
func (m *Manager) Start(kind string, items []string, fn Func) Job {
	workers := m.Workers
	if workers <= 0 {
		workers = 4
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &entry{cancel: cancel, job: Job{ID: newID(), Kind: kind, State: Running, Total: len(items), Started: time.Now().Unix()}}
	m.mu.Lock()
	if m.jobs == nil {
		m.jobs = map[string]*entry{}
	}
	m.jobs[e.job.ID] = e
	m.order = append(m.order, e.job.ID)
	m.pruneLocked()
	m.mu.Unlock()
	snap := e.snapshot()
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
		if m.OnProgress == nil {
			return
		}
		e.mu.Lock()
		due := force || time.Since(lastNotify) > 250*time.Millisecond
		if due {
			lastNotify = time.Now()
		}
		e.mu.Unlock()
		if due {
			m.OnProgress(e.snapshot())
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

// Cancel stops a running job (items already started finish).
func (m *Manager) Cancel(id string) bool {
	m.mu.Lock()
	e := m.jobs[id]
	m.mu.Unlock()
	if e == nil {
		return false
	}
	e.cancel()
	return true
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
