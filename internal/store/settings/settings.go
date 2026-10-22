// Package settings owns accounts/settings.json, the single JSON document that
// carries every panel-managed setting. The file format is shared with the
// legacy Python gateway; unknown keys always survive a rewrite.
//
// Reads normally use an in-memory snapshot. Once per CheckInterval a reader
// synchronously checks the file or backend; concurrent refreshes are coalesced.
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
	"reflect"
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
	dev        deviceTokenCache
	path       string
	loadFn     func() (Doc, error)
	saveFn     func(Doc) error
	updateFn   func(func(Doc) error) (Doc, error)
	lastCheck  atomic.Int64 // unix nanos of the last stat
	revSeq     atomic.Uint64
	mu         sync.Mutex    // serialises writers
	refreshMu  sync.Mutex    // serialises refresh I/O, independent of committed writes
	refreshSeq atomic.Uint64 // completed refreshes, including unchanged/error results
	snap       atomic.Pointer[snapshot]
}

// Open returns a Store for dir/settings.json. The file need not exist.
func Open(accountsDir string) *Store {
	return &Store{path: filepath.Join(accountsDir, "settings.json")}
}

// OpenWith returns a Store backed by external load/save functions (for example
// PostgreSQL). load is polled at most once per CheckInterval; save replaces the
// whole document atomically. path is empty in this mode.
func OpenWith(load func() (Doc, error), save func(Doc) error) *Store {
	return &Store{loadFn: load, saveFn: save}
}

// OpenWithUpdater uses a backend transaction for read/modify/write. update must
// load fresh state, call fn exactly once, and return the committed document. It
// must not retry fn: callbacks may generate secrets or have other side effects.
func OpenWithUpdater(load func() (Doc, error), save func(Doc) error, update func(func(Doc) error) (Doc, error)) *Store {
	return &Store{loadFn: load, saveFn: save, updateFn: update}
}

// OpenWithUpdaterInitial starts with an already successfully loaded backend
// document. Publishing it before the first read avoids discarding known auth
// configuration if the backend becomes unavailable immediately after startup.
// The initial decoded JSON document is recursively copied, so its maps/slices
// remain owned by the caller; other values must be immutable JSON scalars.
// Backend polling resumes after CheckInterval, and committed updates remain
// immediately visible, exactly as with OpenWithUpdater.
func OpenWithUpdaterInitial(initial Doc, load func() (Doc, error), save func(Doc) error, update func(func(Doc) error) (Doc, error)) *Store {
	s := OpenWithUpdater(load, save, update)
	doc := cloneInitialValue(initial).(Doc)
	s.snap.Store(&snapshot{rev: s.revSeq.Add(1), doc: doc, exists: true})
	s.lastCheck.Store(time.Now().UnixNano())
	return s
}

func cloneInitialValue(v any) any {
	switch v := v.(type) {
	case Doc:
		out := make(Doc, len(v))
		for k, value := range v {
			out[k] = cloneInitialValue(value)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, value := range v {
			out[k] = cloneInitialValue(value)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, value := range v {
			out[i] = cloneInitialValue(value)
		}
		return out
	default:
		return v
	}
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

func (s *Store) refresh() *snapshot { return s.refreshWithForce(false) }

// force is for legacy read/modify/write: it must load fresh state after any
// in-flight read, not join a result that started before the mutation.
func (s *Store) refreshWithForce(force bool) *snapshot {
	completed := s.refreshSeq.Load()
	cur := s.snap.Load()
	// The request path must not hit the filesystem on every call: a stat per
	// request showed up as hundreds of milliseconds of tail latency under load.
	// A due reader checks synchronously: do not make auth/revocation reads
	// unconditionally stale while a background poll runs. Our writes are
	// immediately visible (or invalidate the snapshot in file/legacy mode).
	now := time.Now().UnixNano()
	if !force && cur != nil && now-s.lastCheck.Load() < int64(CheckInterval) {
		return cur
	}
	// Do not hold the writer mutex across backend I/O. A committed Update
	// must be able to publish while an older refresh is blocked; CAS below
	// fences that older result. Cold callers wait here rather than receiving
	// an empty document (which can disable API-key authentication).
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	cur = s.snap.Load()
	now = time.Now().UnixNano()
	// Join the refresh completed while we waited, even if its I/O took more
	// than one interval. A later caller can check again immediately, but a
	// burst of waiters must not each repeat the same slow/error/unchanged read.
	if !force && cur != nil && (s.refreshSeq.Load() != completed || now-s.lastCheck.Load() < int64(CheckInterval)) {
		return cur
	}
	// Keep the interval anchored to the START of I/O, not its completion:
	// slow loads must not add another CheckInterval of stale authorization.
	s.lastCheck.Store(now)
	defer s.refreshSeq.Add(1)
	if s.loadFn != nil {
		doc, err := s.loadFn()
		if err != nil {
			if cur != nil {
				return s.latestSnapshot(cur)
			}
			n := &snapshot{rev: s.revSeq.Add(1), doc: Doc{}}
			if s.snap.CompareAndSwap(cur, n) {
				return n
			}
			return s.latestSnapshot(n)
		}
		if cur != nil && cur.exists && reflect.DeepEqual(cur.doc, doc) {
			return s.latestSnapshot(cur)
		}
		n := &snapshot{rev: s.revSeq.Add(1), doc: doc, exists: true}
		// A read started before a successful mutation must not publish its older
		// document over the committed snapshot installed by that mutation.
		if s.snap.CompareAndSwap(cur, n) {
			return n
		}
		return s.latestSnapshot(n)
	}
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

// latestSnapshot tolerates legacy Update invalidating the cache between loads.
func (s *Store) latestSnapshot(fallback *snapshot) *snapshot {
	if current := s.snap.Load(); current != nil {
		return current
	}
	return fallback
}

// Save replaces the whole document atomically.
func (s *Store) Save(doc Doc) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(doc)
}

func (s *Store) saveLocked(doc Doc) error {
	if s.saveFn != nil {
		if err := s.saveFn(doc); err != nil {
			return err
		}
		s.snap.Store(&snapshot{rev: s.revSeq.Add(1), doc: doc, exists: true})
		s.lastCheck.Store(time.Now().UnixNano())
		return nil
	}
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
	if s.updateFn != nil {
		doc, err := s.updateFn(fn)
		if err != nil {
			return err
		}
		s.snap.Store(&snapshot{rev: s.revSeq.Add(1), doc: doc, exists: true})
		s.lastCheck.Store(time.Now().UnixNano())
		return nil
	}
	s.snap.Store(nil)
	fresh := s.refreshWithForce(true)
	doc := make(Doc, len(fresh.doc))
	for k, v := range fresh.doc {
		doc[k] = v
	}
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

func floatOf(v any) float64 {
	switch t := v.(type) {
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return f
		}
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f
		}
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
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
