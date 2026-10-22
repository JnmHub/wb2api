// Package panelauth implements the dashboard's login: persistent sessions
// (they survive browser restarts, gateway restarts and container rebuilds),
// cookie transport with CSRF protection, login rate limiting and a device list.
//
// The legacy gateway kept the session token in the browser's sessionStorage and
// the server-side table only in memory; closing the tab or restarting the
// process logged everybody out.
package panelauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"wb2api/internal/store/accounts"
)

// SessionFile is the file name inside the accounts directory.
const SessionFile = "panel_sessions.json"

// Default lifetimes (sliding: every use pushes the expiry out again).
const (
	RememberTTL = 30 * 24 * time.Hour
	ShortTTL    = 12 * time.Hour
	touchEvery  = time.Minute
)

// Session is one logged-in browser. Only the hash of the token is stored, so a
// leaked state file cannot be replayed as a cookie.
type Session struct {
	ID        string    `json:"id"` // hex SHA-256 of the token
	CSRF      string    `json:"csrf"`
	Created   time.Time `json:"created"`
	LastSeen  time.Time `json:"last_seen"`
	Expires   time.Time `json:"expires"`
	Remember  bool      `json:"remember"`
	UserAgent string    `json:"ua,omitempty"`
	IP        string    `json:"ip,omitempty"`
}

// Short is a stable, non-secret handle for the device list.
func (s Session) Short() string { return s.ID[:12] }

type fileDoc struct {
	Version  int       `json:"version"`
	Sessions []Session `json:"sessions"`
}

// Backend stores panel sessions outside the gateway process. A shared backend
// (PostgreSQL) keeps every replica's device list and revocations consistent.
// Implementations must be safe for concurrent use.
type Backend interface {
	Get(id string) (Session, bool, error)
	List() ([]Session, error)
	Put(Session) error
	Delete(id string) error
	DeleteAllExcept(keep string) (int, error)
}

// TouchBackend atomically extends an existing session. Implementations must
// update no other fields and return false when the row was revoked meanwhile.
type TouchBackend interface {
	Touch(id string, lastSeen, expires time.Time) (Session, bool, error)
}

// Store keeps sessions in memory and mirrors them to disk. Creating and
// revoking sessions is written through immediately; sliding-expiry touches are
// batched. With a Backend, the backend is the source of truth and no file or
// in-memory cache is used.
type Store struct {
	path    string
	backend Backend
	Now     func() time.Time

	mu       sync.Mutex
	sessions map[string]*Session
	dirty    bool
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// OpenStore loads (or creates) the session file in dir and drops expired entries.
func OpenStore(dir string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, SessionFile), sessions: map[string]*Session{}}
	if raw, err := os.ReadFile(s.path); err == nil {
		var doc fileDoc
		if json.Unmarshal(raw, &doc) == nil {
			for i := range doc.Sessions {
				ss := doc.Sessions[i]
				if len(ss.ID) >= 12 {
					s.sessions[ss.ID] = &ss
				}
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	s.pruneLocked()
	return s, nil
}

// OpenShared opens a session store backed by a shared Backend. The backend is
// the source of truth; the caller is responsible for any one-time file import.
func OpenShared(b Backend) (*Store, error) {
	if b == nil {
		return nil, errors.New("nil panel session backend")
	}
	s := &Store{backend: b, Now: time.Now}
	if _, err := b.List(); err != nil {
		return nil, err
	}
	return s, nil
}

// ReadFileSessions reads the legacy panel_sessions.json without pruning it.
// A missing file is not an error.
func ReadFileSessions(dir string) ([]Session, error) {
	raw, err := os.ReadFile(filepath.Join(dir, SessionFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc fileDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return doc.Sessions, nil
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func ttl(remember bool) time.Duration {
	if remember {
		return RememberTTL
	}
	return ShortTTL
}

func (s *Store) pruneLocked() {
	now := s.now()
	for id, ss := range s.sessions {
		if !ss.Expires.After(now) {
			delete(s.sessions, id)
			s.dirty = true
		}
	}
}

func (s *Store) flushLocked() error {
	list := make([]Session, 0, len(s.sessions))
	for _, ss := range s.sessions {
		list = append(list, *ss)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Created.Before(list[j].Created) })
	raw, err := json.MarshalIndent(fileDoc{Version: 1, Sessions: list}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if err := accounts.WriteFileAtomic(s.path, raw); err != nil {
		return err
	}
	_ = os.Chmod(s.path, 0o600)
	s.dirty = false
	return nil
}

// Create opens a session and returns the cookie token (shown only once).
func (s *Store) Create(remember bool, ua, ip string) (token string, sess Session, err error) {
	token = randToken(32)
	now := s.now()
	ss := &Session{
		ID: hashToken(token), CSRF: randToken(24), Created: now, LastSeen: now,
		Expires: now.Add(ttl(remember)), Remember: remember, UserAgent: clip(ua, 200), IP: ip,
	}
	if s.backend != nil {
		if err := s.backend.Put(*ss); err != nil {
			return "", Session{}, err
		}
		return token, *ss, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	s.sessions[ss.ID] = ss
	return token, *ss, s.flushLocked()
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// Lookup validates a token and slides its expiry. ok is false for unknown or
// expired tokens.
func (s *Store) Lookup(token string) (Session, bool) {
	ss, _, ok := s.LookupTouch(token)
	return ss, ok
}

// LookupTouch is Lookup that also reports whether the expiry was extended
// (callers then re-issue the cookie so its lifetime slides too).
func (s *Store) LookupTouch(token string) (Session, bool, bool) {
	if token == "" {
		return Session{}, false, false
	}
	id := hashToken(token)
	if s.backend != nil {
		return s.lookupShared(id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.sessions[id]
	if !ok {
		return Session{}, false, false
	}
	now := s.now()
	if !ss.Expires.After(now) {
		delete(s.sessions, id)
		s.dirty = true
		return Session{}, false, false
	}
	touched := false
	if now.Sub(ss.LastSeen) >= touchEvery {
		ss.LastSeen = now
		ss.Expires = now.Add(ttl(ss.Remember))
		s.dirty = true
		touched = true
	}
	return *ss, touched, true
}

func (s *Store) lookupShared(id string) (Session, bool, bool) {
	ss, ok, err := s.backend.Get(id)
	if err != nil || !ok {
		return Session{}, false, false
	}
	now := s.now()
	if !ss.Expires.After(now) {
		_ = s.backend.Delete(id)
		return Session{}, false, false
	}
	if now.Sub(ss.LastSeen) < touchEvery {
		return ss, false, true
	}
	ss.LastSeen = now
	ss.Expires = now.Add(ttl(ss.Remember))
	if toucher, ok := s.backend.(TouchBackend); ok {
		updated, touched, err := toucher.Touch(id, ss.LastSeen, ss.Expires)
		if err != nil {
			return ss, false, true
		}
		if !touched {
			return Session{}, false, false
		}
		return updated, true, true
	}
	if err := s.backend.Put(ss); err != nil {
		// The session is still valid; a failed touch only means the cookie's
		// sliding expiry is retried on the next request.
		return ss, false, true
	}
	return ss, true, true
}

// Revoke ends the session for token.
func (s *Store) Revoke(token string) {
	if s.backend != nil {
		_ = s.backend.Delete(hashToken(token))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[hashToken(token)]; ok {
		delete(s.sessions, hashToken(token))
		_ = s.flushLocked()
	}
}

// RevokeByShort ends the session with the given short handle; reports whether
// one existed.
func (s *Store) RevokeByShort(short string) bool {
	if len(short) < 8 {
		return false
	}
	if s.backend != nil {
		for _, ss := range s.List() {
			if strings.HasPrefix(ss.ID, short) {
				return s.backend.Delete(ss.ID) == nil
			}
		}
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.sessions {
		if len(short) >= 8 && id[:len(short)] == short {
			delete(s.sessions, id)
			_ = s.flushLocked()
			return true
		}
	}
	return false
}

// RevokeAllExcept ends every session except the one for keepToken ("" = all).
func (s *Store) RevokeAllExcept(keepToken string) int {
	keep := ""
	if keepToken != "" {
		keep = hashToken(keepToken)
	}
	if s.backend != nil {
		n, err := s.backend.DeleteAllExcept(keep)
		if err != nil {
			return 0
		}
		return n
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id := range s.sessions {
		if id != keep {
			delete(s.sessions, id)
			n++
		}
	}
	if n > 0 {
		_ = s.flushLocked()
	}
	return n
}

// List returns all live sessions, newest activity first.
func (s *Store) List() []Session {
	if s.backend != nil {
		list, err := s.backend.List()
		if err != nil {
			return nil
		}
		now := s.now()
		out := make([]Session, 0, len(list))
		for _, ss := range list {
			if ss.Expires.After(now) {
				out = append(out, ss)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	out := make([]Session, 0, len(s.sessions))
	for _, ss := range s.sessions {
		out = append(out, *ss)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

// Flush writes pending sliding-expiry updates.
func (s *Store) Flush() error {
	if s.backend != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	return s.flushLocked()
}

// StartFlusher batches touch updates to disk every interval until Close.
func (s *Store) StartFlusher(interval time.Duration) {
	if s.backend != nil {
		return
	}
	s.mu.Lock()
	if s.stopCh != nil {
		s.mu.Unlock()
		return
	}
	s.stopCh, s.doneCh = make(chan struct{}), make(chan struct{})
	stop, done := s.stopCh, s.doneCh
	s.mu.Unlock()
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				_ = s.Flush()
				return
			case <-t.C:
				_ = s.Flush()
			}
		}
	}()
}

// Close stops the flusher and writes pending state.
func (s *Store) Close() error {
	if s.backend != nil {
		return nil
	}
	s.mu.Lock()
	stop, done := s.stopCh, s.doneCh
	s.stopCh = nil
	s.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
	return s.Flush()
}

// csrfEqual compares in constant time.
func csrfEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
