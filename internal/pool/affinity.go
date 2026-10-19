package pool

import (
	"sync"
	"time"
)

// SessionAffinity remembers which accounts served a conversation, so the next
// turn can go back to one that already holds its prompt prefix upstream (the
// upstream cache is per account).
//
// Each conversation keeps a short most-recently-used list rather than a single
// account: when the front entry is unusable the next pick prefers an account the
// conversation has already warmed over a cold one. Every entry expires on its
// own clock.
type SessionAffinity struct {
	TTL        time.Duration
	MaxEntries int
	History    int

	mu       sync.Mutex
	bindings map[string][]affEntry
}

type affEntry struct {
	uid     string
	expires time.Time
}

// NewSessionAffinity returns the legacy defaults: 2 h TTL, 5000 conversations,
// 3 accounts of history.
func NewSessionAffinity() *SessionAffinity {
	return &SessionAffinity{TTL: 2 * time.Hour, MaxEntries: 5000, History: 3, bindings: map[string][]affEntry{}}
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
	}
	return out
}

// Recent lists the conversation's unexpired accounts, most recent first.
func (s *SessionAffinity) Recent(key string) []string {
	if key == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.live(key, time.Now())
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
	defer s.mu.Unlock()
	now := time.Now()
	live := s.live(key, now)
	if len(live) == 0 {
		return ""
	}
	live[0].expires = now.Add(s.TTL)
	return live[0].uid
}

// Bind makes uid the front entry, deduplicated and capped at History.
func (s *SessionAffinity) Bind(key, uid string) {
	if key == "" || uid == "" {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bindings) >= s.MaxEntries {
		for k, v := range s.bindings {
			if len(v) == 0 || !v[0].expires.After(now) {
				delete(s.bindings, k)
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
}

// Demote drops one account from the conversation, keeping the others: the
// failure paths use it so the failed account is not handed the next attempt
// while the warm ones before it are still tried first.
func (s *SessionAffinity) Demote(key, uid string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
}

// Len is the number of tracked conversations.
func (s *SessionAffinity) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bindings)
}
