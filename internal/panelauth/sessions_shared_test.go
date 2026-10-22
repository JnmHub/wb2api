package panelauth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type memBackend struct {
	mu   sync.Mutex
	data map[string]Session
}

func newMemBackend() *memBackend { return &memBackend{data: map[string]Session{}} }

func (m *memBackend) Get(id string) (Session, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ss, ok := m.data[id]
	return ss, ok, nil
}

func (m *memBackend) List() ([]Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Session, 0, len(m.data))
	for _, ss := range m.data {
		out = append(out, ss)
	}
	return out, nil
}

func (m *memBackend) Put(ss Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[ss.ID] = ss
	return nil
}

func (m *memBackend) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, id)
	return nil
}

func (m *memBackend) DeleteAllExcept(keep string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id := range m.data {
		if id != keep {
			delete(m.data, id)
			n++
		}
	}
	return n, nil
}

type revokeDuringTouchBackend struct {
	*memBackend
	entered chan struct{}
	release chan struct{}
}

func (b *revokeDuringTouchBackend) Touch(id string, lastSeen, expires time.Time) (Session, bool, error) {
	close(b.entered)
	<-b.release
	b.mu.Lock()
	defer b.mu.Unlock()
	ss, ok := b.data[id]
	if !ok {
		return Session{}, false, nil
	}
	ss.LastSeen, ss.Expires = lastSeen, expires
	b.data[id] = ss
	return ss, true, nil
}

func TestSharedTouchCannotResurrectRevokedSession(t *testing.T) {
	now := time.Now()
	be := &revokeDuringTouchBackend{memBackend: newMemBackend(), entered: make(chan struct{}), release: make(chan struct{})}
	s, err := OpenShared(be)
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return now }
	token, _, err := s.Create(true, "ua", "ip")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	result := make(chan struct {
		s           Session
		touched, ok bool
	}, 1)
	go func() {
		ss, touched, ok := s.LookupTouch(token)
		result <- struct {
			s           Session
			touched, ok bool
		}{ss, touched, ok}
	}()
	<-be.entered
	s.Revoke(token)
	close(be.release)
	got := <-result
	if got.ok || got.touched {
		t.Fatalf("revoked session returned after blocked touch: %+v touched=%v ok=%v", got.s, got.touched, got.ok)
	}
	if _, ok, err := be.Get(hashToken(token)); err != nil || ok {
		t.Fatalf("revoked session resurrected: ok=%v err=%v", ok, err)
	}
}

func TestSharedStoreLifecycle(t *testing.T) {
	now := time.Now()
	be := newMemBackend()
	s, err := OpenShared(be)
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return now }

	token, created, err := s.Create(false, "ua", "1.2.3.4")
	if err != nil || token == "" || created.ID == "" {
		t.Fatalf("create = %q %+v %v", token, created, err)
	}
	if _, ok := s.Lookup(token); !ok {
		t.Fatal("new session not found")
	}
	now = now.Add(2 * time.Minute)
	got, touched, ok := s.LookupTouch(token)
	if !ok || !touched {
		t.Fatalf("touch = %+v touched=%v ok=%v", got, touched, ok)
	}
	if got.Expires.Sub(now) < ShortTTL-time.Second {
		t.Fatalf("expiry not slid: %v", got.Expires.Sub(now))
	}
	stored, ok, _ := be.Get(created.ID)
	if !ok || !stored.LastSeen.Equal(now) {
		t.Fatalf("touch not persisted: %+v ok=%v", stored, ok)
	}

	s.Revoke(token)
	if _, ok := s.Lookup(token); ok {
		t.Fatal("revoked session still valid")
	}
}

func TestSharedStoreRevokeAllAndShort(t *testing.T) {
	be := newMemBackend()
	s, err := OpenShared(be)
	if err != nil {
		t.Fatal(err)
	}
	t1, a, _ := s.Create(false, "", "")
	t2, _, _ := s.Create(false, "", "")
	if n := s.RevokeAllExcept(t1); n != 1 {
		t.Fatalf("revoke all except = %d, want 1", n)
	}
	if _, ok := s.Lookup(t2); ok {
		t.Fatal("non-kept session still valid")
	}
	if _, ok := s.Lookup(t1); !ok {
		t.Fatal("kept session was revoked")
	}
	_, d, _ := s.Create(false, "", "")
	if !s.RevokeByShort(d.Short()) {
		t.Fatal("revoke by short returned false")
	}
	if len(s.List()) != 1 || s.List()[0].ID != a.ID {
		t.Fatalf("list after revoke = %+v", s.List())
	}
}

func TestSharedStoreExpiresSession(t *testing.T) {
	now := time.Now()
	be := newMemBackend()
	s, _ := OpenShared(be)
	s.Now = func() time.Time { return now }
	token, created, _ := s.Create(false, "", "")
	now = now.Add(ShortTTL + time.Second)
	if _, ok := s.Lookup(token); ok {
		t.Fatal("expired session accepted")
	}
	if _, ok, _ := be.Get(created.ID); ok {
		t.Fatal("expired session not deleted")
	}
}

func TestReadFileSessions(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	doc := fileDoc{Version: 1, Sessions: []Session{{
		ID: "0123456789abcdef", CSRF: "csrf", Created: now, LastSeen: now,
		Expires: now.Add(RememberTTL), Remember: true,
	}}}
	raw, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(dir, SessionFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFileSessions(dir)
	if err != nil || len(got) != 1 || got[0].ID != "0123456789abcdef" {
		t.Fatalf("read = %+v, %v", got, err)
	}
	got, err = ReadFileSessions(t.TempDir())
	if err != nil || len(got) != 0 {
		t.Fatalf("missing file = %+v, %v", got, err)
	}
}
