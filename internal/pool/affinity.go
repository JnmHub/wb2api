package pool

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// AffinityStore shares conversation bindings between hub replicas. It is
// optional: without a store SessionAffinity keeps its historical in-process
// behavior.
type AffinityStore interface {
	Recent(ctx context.Context, key string, limit int) ([]string, error)
	Bind(ctx context.Context, key, uid string, ttl time.Duration, history int) error
	Demote(ctx context.Context, key, uid string) error
}

// SessionAffinity remembers which accounts served a conversation, so the next
// turn can go back to one that already holds its prompt prefix upstream (the
// upstream cache is per account).
//
// Each conversation keeps a short most-recently-used list rather than a single
// account: when the front entry is unusable the next pick prefers an account the
// conversation has already warmed over a cold one. Every entry expires on its
// own clock.
//
// When Store is set the local map is a short-lived cache in front of it. Reads
// refresh on a miss, writes are queued so binding a conversation never waits on
// Redis, and failures fall back to the local view instead of failing a request.
type SessionAffinity struct {
	TTL        time.Duration
	MaxEntries int
	History    int

	// CacheTTL bounds how long a replica trusts its local copy of a binding.
	CacheTTL time.Duration
	// Timeout bounds one store read or demote.
	Timeout time.Duration

	mu       sync.Mutex
	bindings map[string][]affEntry
	loadedAt map[string]time.Time
	versions map[string]uint64
	store    AffinityStore

	syncCh  chan func()
	dropped atomic.Int64
	errors  atomic.Int64
}

type affEntry struct {
	uid     string
	expires time.Time
}

// AffinitySyncStats reports the shared-affinity write path. Pending is the
// number of queued bind writes; Dropped counts writes shed because that queue
// was full; Errors counts failed store operations.
type AffinitySyncStats struct {
	Shared  bool
	Pending int
	Dropped int64
	Errors  int64
}

// NewSessionAffinity returns the legacy defaults: 2 h TTL, 5000 conversations,
// 3 accounts of history. CacheTTL/Timeout apply only when a store is attached.
func NewSessionAffinity() *SessionAffinity {
	s := &SessionAffinity{
		TTL: 2 * time.Hour, MaxEntries: 5000, History: 3,
		CacheTTL: 5 * time.Second, Timeout: 250 * time.Millisecond,
		bindings: map[string][]affEntry{}, loadedAt: map[string]time.Time{}, versions: map[string]uint64{},
		syncCh: make(chan func(), 2048),
	}
	go s.syncWorker()
	return s
}

// SetStore enables cross-replica bindings. It must be called before serving
// traffic; later calls only replace the store handle.
func (s *SessionAffinity) SetStore(store AffinityStore) {
	if store == nil {
		return
	}
	s.mu.Lock()
	s.store = store
	s.mu.Unlock()
}

func (s *SessionAffinity) syncWorker() {
	for fn := range s.syncCh {
		fn()
	}
}

func (s *SessionAffinity) noteError() {
	s.errors.Add(1)
}

func (s *SessionAffinity) enqueue(fn func()) {
	if s.syncCh == nil {
		s.noteError()
		return
	}
	select {
	case s.syncCh <- fn:
	default:
		s.dropped.Add(1)
	}
}

// SyncStats is a lock-free snapshot of the shared store queue.
func (s *SessionAffinity) SyncStats() AffinitySyncStats {
	s.mu.Lock()
	shared := s.store != nil
	s.mu.Unlock()
	pending := 0
	if s.syncCh != nil {
		pending = len(s.syncCh)
	}
	return AffinitySyncStats{
		Shared: shared, Pending: pending,
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

func (s *SessionAffinity) live(key string, now time.Time) []affEntry {
	entries := s.bindings[key]
	out := entries[:0:0]
	for _, e := range entries {
		if e.expires.After(now) {
			out = append(out, e)
		}
	}
	if len(out) > 0 {
		s.bindings[key] = out
	} else {
		delete(s.bindings, key)
		delete(s.loadedAt, key)
		delete(s.versions, key)
	}
	return out
}

func (s *SessionAffinity) freshLocked(key string, now time.Time) bool {
	at, ok := s.loadedAt[key]
	return ok && now.Sub(at) < s.cacheTTL()
}

func affinityEntries(uids []string, ttl time.Duration, history int, now time.Time) []affEntry {
	if history <= 0 {
		history = 3
	}
	entries := make([]affEntry, 0, len(uids))
	seen := make(map[string]struct{}, len(uids))
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

// refresh loads one conversation from the store. Local changes made while the
// read is in flight win over the result, so a Bind/Demote is never undone by a
// concurrent cache fill.
func (s *SessionAffinity) refresh(key string) {
	s.mu.Lock()
	store := s.store
	version := s.versions[key]
	s.mu.Unlock()
	if store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout())
	defer cancel()
	uids, err := store.Recent(ctx, key, s.History)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.versions[key] != version {
		s.loadedAt[key] = now
		return
	}
	if err != nil {
		s.noteError()
		s.loadedAt[key] = now
		return
	}
	s.bindings[key] = affinityEntries(uids, s.TTL, s.History, now)
	s.loadedAt[key] = now
}

// Recent lists the conversation's unexpired accounts, most recent first.
func (s *SessionAffinity) Recent(key string) []string {
	if key == "" {
		return nil
	}
	s.mu.Lock()
	store := s.store
	now := time.Now()
	live := s.live(key, now)
	if store == nil || s.freshLocked(key, now) {
		out := make([]string, len(live))
		for i, e := range live {
			out[i] = e.uid
		}
		s.mu.Unlock()
		return out
	}
	s.mu.Unlock()

	s.refresh(key)

	s.mu.Lock()
	defer s.mu.Unlock()
	live = s.live(key, time.Now())
	out := make([]string, len(live))
	for i, e := range live {
		out[i] = e.uid
	}
	return out
}

// Get returns the account the last turn went to (sliding its expiry).
func (s *SessionAffinity) Get(key string) string {
	if key == "" {
		return ""
	}
	s.mu.Lock()
	store := s.store
	if store == nil {
		now := time.Now()
		live := s.live(key, now)
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
	recent := s.Recent(key)
	if len(recent) == 0 {
		return ""
	}
	s.Bind(key, recent[0])
	return recent[0]
}

// Bind makes uid the front entry, deduplicated and capped at History.
func (s *SessionAffinity) Bind(key, uid string) {
	if key == "" || uid == "" {
		return
	}
	now := time.Now()
	s.mu.Lock()
	if len(s.bindings) >= s.MaxEntries {
		for k, v := range s.bindings {
			if len(v) == 0 || !v[0].expires.After(now) {
				delete(s.bindings, k)
				delete(s.loadedAt, k)
				delete(s.versions, k)
			}
		}
	}
	entries := []affEntry{{uid, now.Add(s.TTL)}}
	for _, e := range s.bindings[key] {
		if e.uid != uid {
			entries = append(entries, e)
		}
	}
	if len(entries) > s.History {
		entries = entries[:s.History]
	}
	s.bindings[key] = entries
	s.loadedAt[key] = now
	s.versions[key]++
	store := s.store
	ttl, history := s.TTL, s.History
	s.mu.Unlock()

	if store == nil {
		return
	}
	s.enqueue(func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.timeout())
		defer cancel()
		if err := store.Bind(ctx, key, uid, ttl, history); err != nil {
			s.noteError()
		}
	})
}

// Demote drops one account from the conversation, keeping the others: the
// failure paths use it so the failed account is not handed the next attempt
// while the warm ones before it are still tried first. The shared write is
// synchronous because leaving a failed account bound on other replicas is worse
// than one short Redis round trip on an already failing request.
func (s *SessionAffinity) Demote(key, uid string) {
	if key == "" {
		return
	}
	now := time.Now()
	s.mu.Lock()
	entries := s.bindings[key]
	var kept []affEntry
	for _, e := range entries {
		if e.uid != uid {
			kept = append(kept, e)
		}
	}
	if len(kept) > 0 {
		s.bindings[key] = kept
	} else {
		delete(s.bindings, key)
	}
	s.loadedAt[key] = now
	s.versions[key]++
	store := s.store
	s.mu.Unlock()

	if store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout())
	defer cancel()
	if err := store.Demote(ctx, key, uid); err != nil {
		s.noteError()
	}
}

// Len is the number of tracked conversations.
func (s *SessionAffinity) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bindings)
}
