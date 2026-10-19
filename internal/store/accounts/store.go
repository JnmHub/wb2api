package accounts

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// NonAccountFiles live next to the credentials but are not accounts.
var NonAccountFiles = map[string]bool{
	"settings.json":       true,
	"panel_sessions.json": true,
	"active_realm.json":   true,
	"model_windows.json":  true,
	"quota_scan.json":     true,
}

// LoadError describes one unreadable account file (loading continues past it).
type LoadError struct {
	File string
	Err  error
}

func (e LoadError) Error() string { return fmt.Sprintf("account %s unreadable: %v", e.File, e.Err) }

func decodeObject(raw []byte) (map[string]any, error) {
	// Tolerate a UTF-8 BOM; hand-edited files on Windows often have one.
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("not a JSON object")
	}
	return m, nil
}

// Load reads every account file in dir, sorted by file name. Unreadable files
// are reported in the second return value but never abort the load. Accounts
// without a uid are skipped, as in the legacy loader.
func Load(dir string, now time.Time) ([]*Record, []LoadError) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".json") || NonAccountFiles[n] {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	var out []*Record
	var errs []LoadError
	for _, n := range names {
		p := filepath.Join(dir, n)
		raw, err := os.ReadFile(p)
		if err != nil {
			errs = append(errs, LoadError{n, err})
			continue
		}
		m, err := decodeObject(raw)
		if err != nil {
			errs = append(errs, LoadError{n, err})
			continue
		}
		rec := FromMap(m, p, now)
		if rec.UID == "" {
			continue
		}
		out = append(out, rec)
	}
	return out, errs
}

// ordered builds the persisted document in the legacy key order.
func (r *Record) ordered() ([]string, map[string]any) {
	enabled := r.Enabled
	if enabled == nil {
		enabled = true
	}
	weight := r.Weight
	if weight == 0 {
		weight = 1
	}
	dm := r.DisabledModels
	if dm == nil {
		dm = []string{}
	}
	caps := map[string]ModelCap{}
	for k, v := range r.ModelCaps {
		caps[k] = v
	}
	keys := []string{
		"uid", "nickname", "domain", "realm", "platform", "product", "enterpriseId",
		"accessToken", "refreshToken", "expiresAt", "addedAt", "source", "proxySlot",
		"proxy", "enabled", "weight", "note", "disabledModels", "restrictedUntil",
		"credits", "lastCheckin", "lastDailyChat", "lastActivityReport", "modelCaps",
	}
	vals := map[string]any{
		"uid": r.UID, "nickname": r.Nickname, "domain": r.Domain, "realm": r.Realm,
		"platform": r.Platform, "product": r.Product, "enterpriseId": r.EnterpriseID,
		"accessToken": r.AccessToken, "refreshToken": r.RefreshToken,
		"expiresAt": r.ExpiresAt, "addedAt": r.AddedAt, "source": r.Source,
		"proxySlot": r.ProxySlot, "proxy": r.ProxyLegacy, "enabled": enabled,
		"weight": weight, "note": r.Note, "disabledModels": dm,
		"restrictedUntil": roundTripNumber(r.RestrictedUntil),
		"credits":         r.Credits, "lastCheckin": r.LastCheckin,
		"lastDailyChat": r.LastDailyChat, "lastActivityReport": r.LastActivityReport,
		"modelCaps": caps,
	}
	if r.Credits == nil {
		vals["credits"] = nil
	}
	// Unknown keys written by something else survive a rewrite, after the
	// known ones, in stable order.
	extra := make([]string, 0, len(r.Extra))
	for k := range r.Extra {
		extra = append(extra, k)
	}
	sort.Strings(extra)
	for _, k := range extra {
		keys = append(keys, k)
		vals[k] = r.Extra[k]
	}
	return keys, vals
}

// Marshal renders the record exactly like the legacy json.dump(indent=2,
// ensure_ascii=False): two-space indent, UTF-8 text, no HTML escaping.
func (r *Record) Marshal() ([]byte, error) {
	keys, vals := r.ordered()
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := marshalNoEscape(vals[k])
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", k, err)
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	var out bytes.Buffer
	if err := json.Indent(&out, buf.Bytes(), "", "  "); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func marshalNoEscape(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

var saveMu sync.Mutex

// Save writes the record into dir atomically (temp file in the same directory,
// fsync, rename) and sets r.Path. Concurrent saves are serialised.
func Save(dir string, r *Record) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name, ok := r.FileName()
	if !ok {
		var b [16]byte
		_, _ = rand.Read(b[:])
		name = hex.EncodeToString(b[:]) + ".json"
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	path := filepath.Join(absDir, name)
	data, err := r.Marshal()
	if err != nil {
		return "", err
	}
	saveMu.Lock()
	defer saveMu.Unlock()
	if err := writeFileAtomic(path, data); err != nil {
		return "", err
	}
	r.Path = path
	return path, nil
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// WriteFileAtomic is exported for other stores (settings, sessions) that need
// the same crash-safe replace semantics.
func WriteFileAtomic(path string, data []byte) error { return writeFileAtomic(path, data) }

// Delete removes the record's file if it exists.
func Delete(r *Record) error {
	if r.Path == "" {
		return nil
	}
	if err := os.Remove(r.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
