package pool

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// AffinityStore shares conversation bindings between hub replicas. Implementations
// must honor context cancellation. Existing stores remain supported; only the
// optional VersionedAffinityStore interface provides distributed write fencing.
type AffinityStore interface {
	Recent(ctx context.Context, key string, limit int) ([]string, error)
	Bind(ctx context.Context, key, uid string, ttl time.Duration, history int) error
	Demote(ctx context.Context, key, uid string) error
}

// VersionedAffinityStore atomically reads an opaque generation and checks that
// exact observation on Bind. Demote must advance its generation atomically with
// removal. BindVersion must reject expired tokens, never reacquire one. Stores
// must bound metadata retention and prevent expiry/recreation ABA (e.g. a random
// incarnation plus generation). A false result is normal stale-write rejection.
type VersionedAffinityStore interface {
	AffinityStore
	RecentVersion(ctx context.Context, key string, limit int) ([]string, string, error)
	BindVersion(ctx context.Context, key, uid, version string, ttl time.Duration, history int) (bool, error)
}

// AffinityObservation is request-scoped: capture before upstream usage and pass
// unchanged to BindObserved on acceptance. It must not be replaced by a later
// read to make a stale completion succeed. Zero observations cannot share writes.
// Private local identity protects local Demote/eviction too, without an unbounded
// per-key epoch map; distributed ordering is exclusively the store's token.
type AffinityObservation struct {
	owner    *SessionAffinity
	key      string
	identity *affIdentity
	token    string
}

// Separate from the LRU node: a long-lived request must not retain its entries
// or linked list after eviction. Nonzero size guarantees distinct live pointers.
type affIdentity struct{ marker byte }

// SessionAffinity remembers a bounded MRU history of accounts per conversation.
// Each account has its own expiry. With a store, positive, absent and failed
// reads are cached for CacheTTL; store failures fall back to the local view.
// Bind writes are bounded, best-effort and asynchronous; Demote is synchronous.
// Configure the exported fields and SetStore before serving traffic.
type SessionAffinity struct {
	TTL        time.Duration
	MaxEntries int
	History    int
	CacheTTL   time.Duration
	Timeout    time.Duration

	mu      sync.Mutex
	cache   map[string]*affState
	oldest  *affState
	newest  *affState
	store   AffinityStore
	reads   map[string]*affRead
	writes  map[string]map[*affWrite]struct{}
	demotes map[string]*affDemotion

	// Workers start on demand and exit when drained; constructing an affinity
	// table (including a local-only table) does not leave an idle goroutine.
	syncCh     chan func()
	running    bool
	workerDone chan struct{}
	dropped    atomic.Int64
	errors     atomic.Int64
}

type affEntry struct {
	uid     string
	expires time.Time
}

type affState struct {
	key      string
	entries  []affEntry
	loadedAt time.Time
	version  uint64
	identity *affIdentity // replaced on local Demote, retained across ordinary Binds
	token    string       // optional distributed observation, from a successful read only
	older    *affState
	newer    *affState
}

type affRead struct{ done chan struct{} }

type affWrite struct {
	uid     string
	token   string
	stale   bool
	cancel  context.CancelFunc
	barrier *affDemotion
}

type affDemotion struct {
	done chan struct{}
	refs int
}

// AffinitySyncStats reports the best-effort shared write path. Pending excludes
// the currently executing write. Dropped counts full-queue shedding; Errors
// counts failed store operations.
type AffinitySyncStats struct {
	Shared  bool
	Pending int
	Dropped int64
	Errors  int64
}

func NewSessionAffinity() *SessionAffinity {
	return &SessionAffinity{
		TTL: 2 * time.Hour, MaxEntries: 5000, History: 3,
		CacheTTL: 5 * time.Second, Timeout: 250 * time.Millisecond,
		cache: make(map[string]*affState), reads: make(map[string]*affRead),
		writes: make(map[string]map[*affWrite]struct{}), demotes: make(map[string]*affDemotion),
		syncCh: make(chan func(), 2048),
	}
}

// SetStore enables cross-replica bindings. Call before serving traffic.
func (s *SessionAffinity) SetStore(store AffinityStore) {
	if store != nil {
		s.mu.Lock()
		s.store = store
		s.mu.Unlock()
	}
}

func (s *SessionAffinity) syncWorker(done chan struct{}) {
	defer close(done)
	for {
		s.mu.Lock()
		select {
		case fn, ok := <-s.syncCh:
			if !ok {
				s.running = false
				s.mu.Unlock()
				return
			}
			s.mu.Unlock()
			fn()
		default:
			s.running = false
			s.mu.Unlock()
			return
		}
	}
}

// enqueueLocked never blocks on either the queue or the store.
func (s *SessionAffinity) enqueueLocked(fn func()) bool {
	select {
	case s.syncCh <- fn:
		if !s.running {
			s.running = true
			s.workerDone = make(chan struct{})
			go s.syncWorker(s.workerDone)
		}
		return true
	default:
		s.dropped.Add(1)
		return false
	}
}

func (s *SessionAffinity) noteError() { s.errors.Add(1) }

func (s *SessionAffinity) SyncStats() AffinitySyncStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return AffinitySyncStats{
		Shared: s.store != nil, Pending: len(s.syncCh),
		Dropped: s.dropped.Load(), Errors: s.errors.Load(),
	}
}

func (s *SessionAffinity) timeout() time.Duration {
	if s.Timeout <= 0 {
		return 250 * time.Millisecond
	}
	return s.Timeout
}

func (s *SessionAffinity) cacheTTL() time.Duration {
	if s.CacheTTL <= 0 {
		return 5 * time.Second
	}
	return s.CacheTTL
}

func (s *SessionAffinity) capacity() int {
	if s.MaxEntries <= 0 {
		return 5000
	}
	return s.MaxEntries
}

func (s *SessionAffinity) history() int {
	if s.History <= 0 {
		return 3
	}
	return s.History
}

func (s *SessionAffinity) unlinkLocked(e *affState) {
	if e.older != nil {
		e.older.newer = e.newer
	} else {
		s.oldest = e.newer
	}
	if e.newer != nil {
		e.newer.older = e.older
	} else {
		s.newest = e.older
	}
	e.older, e.newer = nil, nil
}

func (s *SessionAffinity) touchLocked(e *affState) {
	if s.newest == e {
		return
	}
	s.unlinkLocked(e)
	e.older, e.newer = s.newest, nil
	if s.newest != nil {
		s.newest.newer = e
	} else {
		s.oldest = e
	}
	s.newest = e
}

// stateLocked is O(1) at a fixed configured capacity, including at saturation.
// Expiry is lazy per key (O(History)); LRU eviction bounds *all* cache metadata,
// including negative/error timestamps. There is no full-map sweep on Bind.
func (s *SessionAffinity) stateLocked(key string) *affState {
	if e := s.cache[key]; e != nil {
		s.touchLocked(e)
		return e
	}
	for len(s.cache) >= s.capacity() {
		e := s.oldest
		s.unlinkLocked(e)
		delete(s.cache, e.key)
	}
	e := &affState{key: key, identity: &affIdentity{}, older: s.newest}
	if s.newest != nil {
		s.newest.newer = e
	} else {
		s.oldest = e
	}
	s.newest = e
	s.cache[key] = e
	return e
}

func liveAffinity(e *affState, now time.Time) []affEntry {
	out := e.entries[:0]
	for _, entry := range e.entries {
		if entry.expires.After(now) {
			out = append(out, entry)
		}
	}
	clear(e.entries[len(out):])
	e.entries = out
	// Do not remove loadedAt when the binding is empty: it also caches a
	// successful absent result, a failed read, or a local Demote.
	return out
}

func affinityUIDs(entries []affEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.uid
	}
	return out
}

func affinityEntries(uids []string, ttl time.Duration, history int, now time.Time) []affEntry {
	if history <= 0 {
		history = 3
	}
	entries := make([]affEntry, 0, min(len(uids), history))
	seen := make(map[string]struct{}, min(len(uids), history))
	for _, uid := range uids {
		if uid == "" {
			continue
		}
		if _, ok := seen[uid]; ok {
			continue
		}
		seen[uid] = struct{}{}
		entries = append(entries, affEntry{uid: uid, expires: now.Add(ttl)})
		if len(entries) >= history {
			break
		}
	}
	return entries
}

// Recent lists unexpired accounts, most recent first.
func (s *SessionAffinity) Recent(key string) []string {
	return s.RecentContext(context.Background(), key)
}

// RecentContext is Recent with request cancellation. Same-key misses share one
// read; waiters can cancel independently. The initiating caller's cancellation
// also cancels its read. There are no helper goroutines and no store I/O under mu.
func (s *SessionAffinity) RecentContext(ctx context.Context, key string) []string {
	uids, _ := s.observeContext(ctx, key, false)
	return uids
}

func (s *SessionAffinity) observationLocked(e *affState) AffinityObservation {
	return AffinityObservation{owner: s, key: e.key, identity: e.identity, token: e.token}
}

// ObserveContext returns candidates and the exact observation to retain for a
// future BindObserved. Cached/partitioned reads remain best effort, not strongly
// consistent; stale observations cannot publish a shared binding after Demote.
func (s *SessionAffinity) ObserveContext(ctx context.Context, key string) ([]string, AffinityObservation) {
	return s.observeContext(ctx, key, true)
}

func (s *SessionAffinity) observeContext(ctx context.Context, key string, observe bool) ([]string, AffinityObservation) {
	if key == "" {
		return nil, AffinityObservation{}
	}
	s.mu.Lock()
	store := s.store
	if !observe && store == nil && s.cache[key] == nil {
		s.mu.Unlock()
		return nil, AffinityObservation{}
	}
	e := s.stateLocked(key)
	now := time.Now()
	live := liveAffinity(e, now)
	if store == nil || s.demotes[key] != nil || (!e.loadedAt.IsZero() && now.Sub(e.loadedAt) < s.cacheTTL()) || ctx.Err() != nil {
		out, observation := affinityUIDs(live), s.observationLocked(e)
		s.mu.Unlock()
		return out, observation
	}
	if read := s.reads[key]; read != nil {
		s.mu.Unlock()
		waitCtx, cancel := context.WithTimeout(ctx, s.timeout())
		select {
		case <-read.done:
		case <-waitCtx.Done():
		}
		cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		if current := s.cache[key]; current != nil {
			return affinityUIDs(liveAffinity(current, time.Now())), s.observationLocked(current)
		}
		return nil, AffinityObservation{}
	}
	// Bound distinct in-flight reads independently of the bounded cache. When
	// saturated, fail open to local affinity instead of creating unbounded work.
	if len(s.reads) >= s.capacity() {
		out, observation := affinityUIDs(live), s.observationLocked(e)
		s.mu.Unlock()
		return out, observation
	}
	read := &affRead{done: make(chan struct{})}
	s.reads[key] = read
	version := e.version
	ttl, history := s.TTL, s.history()
	s.mu.Unlock()

	readCtx, cancel := context.WithTimeout(ctx, s.timeout())
	var uids []string
	var token string
	var err error
	if fenced, ok := store.(VersionedAffinityStore); ok {
		uids, token, err = fenced.RecentVersion(readCtx, key, history)
	} else {
		uids, err = store.Recent(readCtx, key, history)
	}
	cancel()
	now = time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.reads, key)
	defer close(read.done)
	if err != nil {
		s.noteError()
	}
	// Pointer identity prevents an evict/recreate ABA from publishing an old
	// read. Mutation versions exist only inside capacity-bounded cache records.
	if current := s.cache[key]; current == e && e.version == version {
		e.token = "" // a failed refresh never grants permission to share a Bind
		if err == nil {
			e.entries = affinityEntries(uids, ttl, history, now)
			e.token = token
		}
		e.loadedAt = now
	}
	if current := s.cache[key]; current != nil {
		return affinityUIDs(liveAffinity(current, now)), s.observationLocked(current)
	}
	return nil, AffinityObservation{}
}

// Get returns the last account, sliding only that account's expiry.
func (s *SessionAffinity) Get(key string) string {
	if key == "" {
		return ""
	}
	s.mu.Lock()
	if s.store == nil {
		if s.cache[key] == nil {
			s.mu.Unlock()
			return ""
		}
		e := s.stateLocked(key)
		now := time.Now()
		live := liveAffinity(e, now)
		if len(live) == 0 {
			s.mu.Unlock()
			return ""
		}
		live[0].expires = now.Add(s.TTL)
		uid := live[0].uid
		s.mu.Unlock()
		return uid
	}
	s.mu.Unlock()
	recent, observation := s.ObserveContext(context.Background(), key)
	if len(recent) == 0 {
		return ""
	}
	s.BindObserved(key, recent[0], observation)
	return recent[0]
}

// Bind makes uid the front entry, retaining each warm account's original TTL.
// With a versioned store it captures only an existing cached observation, never
// reads a new generation. Request completions should use BindObserved instead.
func (s *SessionAffinity) Bind(key, uid string) {
	s.bind(key, uid, nil)
}

// BindObserved records successful usage against the request's original read.
// A local Demote/eviction invalidates it before even changing the local history.
func (s *SessionAffinity) BindObserved(key, uid string, observation AffinityObservation) {
	s.bind(key, uid, &observation)
}

func (s *SessionAffinity) bind(key, uid string, observation *AffinityObservation) {
	if key == "" || uid == "" {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if observation != nil && (observation.owner != s || observation.key != key || observation.identity == nil || s.cache[key] == nil || s.cache[key].identity != observation.identity) {
		return
	}
	e := s.stateLocked(key)
	token := e.token
	if observation != nil {
		token = observation.token
	}
	history := s.history()
	entries := liveAffinity(e, now)
	if len(entries) > history {
		clear(entries[history:])
		entries = entries[:history]
	}
	at := -1
	for i, old := range entries {
		if old.uid == uid {
			at = i
			break
		}
	}
	if at < 0 {
		if len(entries) < history {
			entries = append(entries, affEntry{})
		}
		at = len(entries) - 1
	}
	copy(entries[1:at+1], entries[:at])
	entries[0] = affEntry{uid, now.Add(s.TTL)}
	e.entries = entries
	e.version++
	store := s.store
	if _, fenced := store.(VersionedAffinityStore); !fenced {
		e.loadedAt = now
	}
	// A local Bind must not refresh a distributed observation's cache age:
	// steady successful traffic otherwise trusts an old generation forever.
	if store == nil {
		return
	}
	if _, fenced := store.(VersionedAffinityStore); fenced && token == "" {
		return // local fallback is allowed; do not manufacture write permission
	}
	write := &affWrite{uid: uid, token: token, barrier: s.demotes[key]}
	if s.writes[key] == nil {
		s.writes[key] = make(map[*affWrite]struct{})
	}
	s.writes[key][write] = struct{}{}
	ttl := s.TTL
	if !s.enqueueLocked(func() { s.syncBind(store, key, write, ttl, history) }) {
		s.removeWriteLocked(key, write)
	}
}

func (s *SessionAffinity) removeWriteLocked(key string, write *affWrite) {
	delete(s.writes[key], write)
	if len(s.writes[key]) == 0 {
		delete(s.writes, key)
	}
}

func (s *SessionAffinity) syncBind(store AffinityStore, key string, write *affWrite, ttl time.Duration, history int) {
	// A Bind made during a synchronous Demote must be applied after it. This
	// wait never holds mu and is bounded even if a store ignores cancellation.
	if write.barrier != nil {
		ctx, cancel := context.WithTimeout(context.Background(), s.timeout())
		select {
		case <-write.barrier.done:
		case <-ctx.Done():
			s.noteError()
			s.mu.Lock()
			s.removeWriteLocked(key, write)
			s.mu.Unlock()
			cancel()
			return
		}
		cancel()
	}
	s.mu.Lock()
	if write.stale {
		s.removeWriteLocked(key, write)
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout())
	write.cancel = cancel
	s.mu.Unlock()
	var err error
	fenced, versioned := store.(VersionedAffinityStore)
	if versioned {
		_, err = fenced.BindVersion(ctx, key, write.uid, write.token, ttl, history)
	} else {
		err = store.Bind(ctx, key, write.uid, ttl, history)
	}
	cancel()
	if err != nil {
		s.noteError()
	}
	s.mu.Lock()
	stale := write.stale
	s.removeWriteLocked(key, write)
	s.mu.Unlock()
	if stale && !versioned {
		// Versioned stores atomically reject late writes. Never compensate them:
		// a blind Demote could delete another replica's valid newer binding.
		// Cancellation alone cannot fence a write already accepted by the
		// backend. Repair after it returns, before the next queued Bind runs.
		// The unchanged store API cannot make this atomic across replicas or
		// guarantee repair after process death / an ambiguous backend failure.
		ctx, cancel := context.WithTimeout(context.Background(), s.timeout())
		defer cancel()
		if err := store.Demote(ctx, key, write.uid); err != nil {
			s.noteError()
		}
	}
}

// Demote removes one account without discarding the rest of the warm history.
// The shared Demote is immediate, not queued behind unrelated Bind traffic.
// Queued older writes for this account are invalidated. Versioned stores fence
// in-flight writes atomically; legacy stores cancel/repair with syncBind's
// documented distributed limitation.
func (s *SessionAffinity) Demote(key, uid string) {
	if key == "" {
		return
	}
	now := time.Now()
	s.mu.Lock()
	e := s.stateLocked(key)
	entries := liveAffinity(e, now)
	kept := entries[:0]
	for _, old := range entries {
		if old.uid != uid {
			kept = append(kept, old)
		}
	}
	clear(entries[len(kept):])
	e.entries, e.loadedAt = kept, now
	e.version++
	e.identity = &affIdentity{}
	e.token = "" // only a later explicit observation can authorize a new Bind
	if _, fenced := s.store.(VersionedAffinityStore); fenced {
		e.loadedAt = time.Time{} // next request must observe the post-Demote generation
	}
	store := s.store
	if store == nil {
		s.mu.Unlock()
		return
	}
	for write := range s.writes[key] {
		if write.uid == uid {
			write.stale = true
			if write.cancel != nil {
				write.cancel()
			}
		}
	}
	barrier := s.demotes[key]
	if barrier == nil {
		barrier = &affDemotion{done: make(chan struct{})}
		s.demotes[key] = barrier
	}
	barrier.refs++
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout())
	if err := store.Demote(ctx, key, uid); err != nil {
		s.noteError()
	}
	cancel()
	s.mu.Lock()
	barrier.refs--
	if barrier.refs == 0 {
		delete(s.demotes, key)
		close(barrier.done)
	}
	s.mu.Unlock()
}

// Len counts conversations with locally cached bindings, excluding absent/error
// cache records. As before, expiry is lazy; Len itself does not sweep the cache.
func (s *SessionAffinity) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.cache {
		if len(e.entries) > 0 {
			n++
		}
	}
	return n
}
