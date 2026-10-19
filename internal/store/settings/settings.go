// Package settings owns accounts/settings.json, the single JSON document that
// carries every panel-managed setting. The file format is shared with the
// legacy Python gateway; unknown keys always survive a rewrite.
//
// Reads are served from an in-memory snapshot that is refreshed only when the
// file's (mtime, size) changes, so the request hot path never touches disk.
package settings

import (
	"bytes"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"wb2api/internal/store/accounts"
)

const (
	// DefaultPanelPassword is the password of a fresh install.
	DefaultPanelPassword = "admin"
	// PBKDF2Rounds is the iteration count written for new passwords.
	PBKDF2Rounds = 120_000
)

// Doc is the decoded settings document (top-level keys only).
type Doc map[string]any

type snapshot struct {
	rev     uint64
	modTime time.Time
	size    int64
	exists  bool
	doc     Doc
}

// Store is the settings.json handle for one accounts directory.
type Store struct {
	dev       deviceTokenCache
	path      string
	lastCheck atomic.Int64 // unix nanos of the last stat
	revSeq    atomic.Uint64
	mu        sync.Mutex // serialises writers
	snap      atomic.Pointer[snapshot]
}

// Open returns a Store for dir/settings.json. The file need not exist.
func Open(accountsDir string) *Store {
	return &Store{path: filepath.Join(accountsDir, "settings.json")}
}

// Path returns the settings file path.
func (s *Store) Path() string { return s.path }

// Get returns the current document. The returned map is a shallow copy: callers
// may modify top-level keys freely, but must treat nested values as read-only.
func (s *Store) Get() Doc {
	snap := s.refresh()
	out := make(Doc, len(snap.doc))
	for k, v := range snap.doc {
		out[k] = v
	}
	return out
}

// View returns the cached document WITHOUT copying. It is for hot-path reads;
// the result must not be mutated.
func (s *Store) View() Doc { return s.refresh().doc }

func (s *Store) refresh() *snapshot {
	cur := s.snap.Load()
	// The request path must not hit the filesystem on every call: a stat per
	// request showed up as hundreds of milliseconds of tail latency under load.
	// External edits are noticed within CheckInterval; our own writes drop the
	// snapshot and are visible immediately.
	now := time.Now().UnixNano()
	if cur != nil && now-s.lastCheck.Load() < int64(CheckInterval) {
		return cur
	}
	s.lastCheck.Store(now)
	info, err := os.Stat(s.path)
	if err != nil {
		if cur != nil && !cur.exists {
			return cur
		}
		n := &snapshot{rev: s.revSeq.Add(1), doc: Doc{}}
		s.snap.Store(n)
		return n
	}
	if cur != nil && cur.exists && cur.size == info.Size() && cur.modTime.Equal(info.ModTime()) {
		return cur
	}
	n := &snapshot{rev: s.revSeq.Add(1), modTime: info.ModTime(), size: info.Size(), exists: true, doc: Doc{}}
	if raw, err := os.ReadFile(s.path); err == nil {
		raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var m map[string]any
		if err := dec.Decode(&m); err == nil && m != nil {
			n.doc = Doc(m)
		}
	}
	s.snap.Store(n)
	return n
}

// Save replaces the whole document atomically.
func (s *Store) Save(doc Doc) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(doc)
}

func (s *Store) saveLocked(doc Doc) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any(doc)); err != nil {
		return err
	}
	if err := accounts.WriteFileAtomic(s.path, bytes.TrimRight(buf.Bytes(), "\n")); err != nil {
		return err
	}
	s.snap.Store(nil) // force a re-read, independent of mtime resolution
	s.lastCheck.Store(0)
	return nil
}

// Update applies fn to a copy of the document and saves the result. fn may
// return an error to abort without writing.
func (s *Store) Update(fn func(Doc) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Store(nil)
	doc := s.Get()
	if err := fn(doc); err != nil {
		return err
	}
	return s.saveLocked(doc)
}

// DeepMerge merges patch into base recursively; sibling keys not mentioned in
// patch survive. Neither input is modified.
func DeepMerge(base, patch map[string]any) map[string]any {
	out := make(map[string]any, len(base))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range patch {
		pm, pok := v.(map[string]any)
		bm, bok := out[k].(map[string]any)
		if pok && bok {
			out[k] = DeepMerge(bm, pm)
		} else {
			out[k] = v
		}
	}
	return out
}

// ---------------------------------------------------------------- password

func digest(password, saltHex string, rounds int) (string, error) {
	salt, err := hex.DecodeString(saltHex)
	if err != nil {
		return "", err
	}
	dk, err := pbkdf2.Key(sha256.New, password, salt, rounds, 32)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(dk), nil
}

func intOf(v any) int {
	switch t := v.(type) {
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return i
		}
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return int(i)
		}
	case float64:
		return int(t)
	case int:
		return t
	}
	return 0
}

// PanelPasswordIsDefault reports whether the panel still uses the default
// password (fresh install or explicitly reset to it).
func (s *Store) PanelPasswordIsDefault() bool {
	d := s.View()
	if h, _ := d["panel_password_hash"].(string); h == "" {
		return true
	}
	b, _ := d["panel_password_default"].(bool)
	return b
}

// VerifyPanelPassword reports whether password opens the panel.
func (s *Store) VerifyPanelPassword(password string) bool {
	d := s.View()
	stored, _ := d["panel_password_hash"].(string)
	if stored == "" {
		return password == DefaultPanelPassword
	}
	if b, _ := d["panel_password_default"].(bool); b {
		return password == DefaultPanelPassword
	}
	salt, _ := d["panel_password_salt"].(string)
	if salt == "" {
		return false
	}
	rounds := intOf(d["panel_password_rounds"])
	if rounds <= 0 {
		rounds = PBKDF2Rounds
	}
	given, err := digest(password, salt, rounds)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(given), []byte(stored))
}

// SetPanelPassword stores a new panel password (never in clear text).
func (s *Store) SetPanelPassword(password string) error {
	return s.Update(func(d Doc) error {
		if password == DefaultPanelPassword {
			delete(d, "panel_password_salt")
			delete(d, "panel_password_rounds")
			d["panel_password_hash"] = ""
			d["panel_password_default"] = true
			return nil
		}
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return err
		}
		salt := hex.EncodeToString(raw[:])
		h, err := digest(password, salt, PBKDF2Rounds)
		if err != nil {
			return err
		}
		d["panel_password_salt"] = salt
		d["panel_password_rounds"] = PBKDF2Rounds
		d["panel_password_hash"] = h
		d["panel_password_default"] = false
		return nil
	})
}

// ErrNotObject is returned when settings.json exists but is not a JSON object.
var ErrNotObject = errors.New("settings.json is not a JSON object")

// CheckInterval is how often the file's mtime is re-checked on reads.
const CheckInterval = 500 * time.Millisecond

// Rev returns a counter that changes whenever the loaded document changes. Hot
// paths use it to cache values derived from the document.
func (s *Store) Rev() uint64 { return s.refresh().rev }
