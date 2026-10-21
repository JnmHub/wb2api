package settings

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// APIKey is one client API key entry.
type APIKey struct {
	ID               string
	Name             string
	Key              string
	Realm            string // "", "intl" or "cn"
	Models           []string
	ConcurrencyLimit int
	TPSLimit         float64 // -1 = off, 0 = follow global, >0 = per-key cap
	Enabled          bool
	CreatedAt        string
	DeletedAt        string
}

func cleanModelPatterns(v any) []string {
	var raw []string
	switch t := v.(type) {
	case string:
		raw = splitAny(t, ",;\n")
	case []any:
		for _, it := range t {
			raw = append(raw, anyToStr(it))
		}
	default:
		return []string{}
	}
	out := []string{}
	seen := map[string]bool{}
	for _, item := range raw {
		p := strings.ToLower(strings.TrimSpace(item))
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func splitAny(s, seps string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune(seps, r) })
}

func anyToStr(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	}
	return fmt.Sprint(v)
}

func truthyStr(v any) string {
	s := anyToStr(v)
	if n, ok := v.(interface{ String() string }); ok {
		s = n.String()
		if s == "0" {
			return ""
		}
	}
	return s
}

// APIKeys returns the configured keys. Deleted entries are hidden unless
// includeDeleted is set. A legacy single `api_key` surfaces as one unbound
// entry, exactly like the legacy loader.
func (s *Store) APIKeys(includeDeleted bool) []APIKey {
	d := s.View()
	stored, isList := d["api_keys"].([]any)
	if isList {
		out := []APIKey{}
		seenSecret := map[string]bool{}
		seenID := map[string]bool{}
		for _, raw := range stored {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			key := strings.TrimSpace(truthyStr(m["key"]))
			deleted := strings.TrimSpace(truthyStr(m["deleted_at"]))
			if key == "" && deleted == "" {
				continue
			}
			realm := strings.ToLower(strings.TrimSpace(truthyStr(m["realm"])))
			if realm != "" && realm != "intl" && realm != "cn" {
				realm = ""
			}
			conc := intOf(m["concurrency_limit"])
			if conc == 0 {
				conc = intOf(m["max_concurrency"])
			}
			if conc < 0 {
				conc = 0
			}
			name := strings.TrimSpace(truthyStr(m["name"]))
			if name == "" {
				name = "未命名"
			}
			enabled := true
			if deleted != "" {
				enabled = false
			} else if b, ok := m["enabled"].(bool); ok && !b {
				enabled = false
			}
			e := APIKey{
				ID: truthyStr(m["id"]), Name: name, Key: key, Realm: realm,
				Models: cleanModelPatterns(m["models"]), ConcurrencyLimit: conc,
				TPSLimit: keyTPSLimit(m), Enabled: enabled,
				CreatedAt: truthyStr(m["created_at"]), DeletedAt: deleted,
			}
			if e.Key != "" {
				if seenSecret[e.Key] {
					continue
				}
				seenSecret[e.Key] = true
			}
			e.ID = uniqueKeyID(e.ID, seenID)
			seenID[e.ID] = true
			if !includeDeleted && e.DeletedAt != "" {
				continue
			}
			out = append(out, e)
		}
		return out
	}
	if set, _ := d["api_key_set"].(bool); set {
		if legacy := strings.TrimSpace(truthyStr(d["api_key"])); legacy != "" {
			return []APIKey{{ID: "legacy", Name: "默认（跟随面板切换）", Key: legacy, Models: []string{}, Enabled: true}}
		}
	}
	return []APIKey{}
}

func keyTPSLimit(m map[string]any) float64 {
	raw, ok := m["tps_limit"]
	if !ok {
		return 0 // inherit global
	}
	f := floatOf(raw)
	if f < -1 {
		return -1
	}
	if f > 1000 {
		return 1000
	}
	return f
}

// uniqueKeyID mirrors _unique_key_id except that, for an empty candidate, it
// derives a deterministic id (the legacy code mints a random one, which would
// make ids flap between reads of the same file).
func uniqueKeyID(candidate string, used map[string]bool) string {
	candidate = strings.TrimSpace(candidate)
	if candidate != "" && !used[candidate] {
		return candidate
	}
	base := candidate
	if base == "" {
		base = "key"
	}
	for i := 2; ; i++ {
		alt := fmt.Sprintf("%s-%d", base, i)
		if !used[alt] {
			return alt
		}
	}
}

// AllowsModel reports whether the key places no model restriction or model
// matches one of its patterns (fnmatch semantics, lower-cased).
func (k APIKey) AllowsModel(model string) bool {
	if len(k.Models) == 0 {
		return true
	}
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return false
	}
	for _, p := range k.Models {
		if FnMatchCase(name, p) {
			return true
		}
	}
	return false
}

var (
	fnMu    sync.RWMutex
	fnCache = map[string]*regexp.Regexp{}
)

// FnMatchCase is Python's fnmatch.fnmatchcase: * ? [seq] [!seq], where, unlike
// path.Match, `*` also matches '/'.
func FnMatchCase(name, pattern string) bool {
	fnMu.RLock()
	re, ok := fnCache[pattern]
	fnMu.RUnlock()
	if !ok {
		re = regexp.MustCompile(translateFnmatch(pattern))
		fnMu.Lock()
		if len(fnCache) < 4096 {
			fnCache[pattern] = re
		}
		fnMu.Unlock()
	}
	return re.MatchString(name)
}

func translateFnmatch(pat string) string {
	var b strings.Builder
	b.WriteString(`(?s)^`)
	rs := []rune(pat)
	for i := 0; i < len(rs); {
		c := rs[i]
		i++
		switch c {
		case '*':
			b.WriteString(`.*`)
		case '?':
			b.WriteString(`.`)
		case '[':
			j := i
			if j < len(rs) && rs[j] == '!' {
				j++
			}
			if j < len(rs) && rs[j] == ']' {
				j++
			}
			for j < len(rs) && rs[j] != ']' {
				j++
			}
			if j >= len(rs) {
				b.WriteString(`\[`)
			} else {
				stuff := string(rs[i:j])
				stuff = strings.ReplaceAll(stuff, `\`, `\\`)
				i = j + 1
				switch {
				case strings.HasPrefix(stuff, "!"):
					stuff = "^" + stuff[1:]
				case strings.HasPrefix(stuff, "^"):
					stuff = `\` + stuff
				}
				b.WriteString("[" + stuff + "]")
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(`$`)
	return b.String()
}
