// Package catalog is the model catalogue: the bundled per-exit tables, the
// live product-config sync, curation/merging, and the rich /v1/models entry
// shape. It is a port of the catalogue parts of legacy wb_proxy.py
// (merge_catalog, curate_remote_catalog, parse_remote_catalog, model_entry),
// wb_catalog.py and wb_modelsdev.py; behaviour is pinned by golden vectors
// from the Python implementation (tools/genvectors/gen_catalog.py).
package catalog

import (
	_ "embed"
	"strings"
	"sync"

	"wb2api/internal/jsonx"
)

//go:embed data/intl.json
var intlJSON []byte

//go:embed data/cn.json
var cnJSON []byte

// Entry is one model of a catalogue: its id and the (ordered) metadata object
// the desktop app's product config carries for it. Meta is never nil.
type Entry struct {
	ID   string
	Meta *jsonx.Obj
}

var (
	staticOnce sync.Once
	staticIntl []Entry
	staticCN   []Entry
)

func parseStatic(raw []byte) []Entry {
	v, err := jsonx.Unmarshal(raw)
	if err != nil {
		panic("catalog: bundled table is not valid JSON: " + err.Error())
	}
	list, _ := v.([]any)
	out := make([]Entry, 0, len(list))
	for _, it := range list {
		if o, ok := it.(*jsonx.Obj); ok {
			if id, _ := o.Str("id"); id != "" {
				out = append(out, Entry{ID: id, Meta: o})
			}
		}
	}
	return out
}

// Static returns the bundled table of an exit ("intl" or "cn"); the result
// must not be modified.
func Static(realm string) []Entry {
	staticOnce.Do(func() {
		staticIntl = parseStatic(intlJSON)
		staticCN = parseStatic(cnJSON)
	})
	if realm == "cn" {
		return staticCN
	}
	return staticIntl
}

// Models that are advertised upstream but are not usable for chat.
var nonChat = map[string]bool{
	"lite": true,
	// virtual aliases / quick presets
	"default-model": true, "fast-model": true, "balanced-model": true,
	"primary-model": true, "deep-model": true,
	// the domestic exit's auto-router entry: shown in the picker but not a
	// model a client can pin
	"auto": true,
}

// IsChatModel reports whether a catalogue id is a usable chat model.
func IsChatModel(id string) bool {
	if id == "" || nonChat[id] {
		return false
	}
	if strings.HasPrefix(id, "codewise-") || strings.HasPrefix(id, "completion-") {
		return false
	}
	for _, s := range []string{"-image-alpha", "-image-alpha-edit", "-taco-completion"} {
		if strings.HasSuffix(id, s) {
			return false
		}
	}
	return true
}

// UI order of the picker; models outside it only appear through extras.
var (
	cnUIOrder = []string{
		"hy4-preview-f", "hy3", "deepseek-v4.1-flash", "glm-5.3", "glm-5.3-flash",
		"glm-5.2", "glm-5.1", "glm-5v-turbo", "minimax-m3", "kimi-k3-1",
		"kimi-k2.8-preview", "kimi-k2.7", "kimi-k2.6", "deepseek-v4-pro",
	}
	intlUIOrder = []string{
		"hy4-preview-f", "hy3", "deepseek-v4.1-flash", "gpt-6-astra", "gpt-5.6-sol",
		"gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.4", "grok-4.7",
		"gemini-3.5-flash", "glm-5.3-flash", "glm-5.3", "glm-5.2", "kimi-k3",
		"kimi-k2.6", "kimi-k2.8-preview",
	}
)

// MergeReasoning merges one model's reasoning block field by field: the live
// block wins, but a block that ends up with supportedEfforts is selectable, so
// a bare `effort` becomes its defaultEffort instead of a pin.
func MergeReasoning(base, live *jsonx.Obj) *jsonx.Obj {
	out := jsonx.NewObj()
	if base != nil {
		out = base.Clone()
	}
	if live != nil {
		live.Each(func(k string, v any) { out.Set(k, v) })
	}
	if jsonx.Truthy(out.Get("supportedEfforts")) && jsonx.Truthy(out.Get("effort")) {
		if !out.Has("defaultEffort") {
			out.Set("defaultEffort", out.Get("effort"))
		}
		out.Delete("effort")
	}
	return out
}

func idSet(entries []Entry) map[string]bool {
	m := make(map[string]bool, len(entries))
	for _, e := range entries {
		m[e.ID] = true
	}
	return m
}

func uiOrder(realm string) []string {
	switch realm {
	case "cn":
		return cnUIOrder
	case "all":
		out := append([]string(nil), intlUIOrder...)
		seen := idSetStrings(intlUIOrder)
		for _, m := range cnUIOrder {
			if !seen[m] {
				out = append(out, m)
			}
		}
		return out
	}
	return intlUIOrder
}

func idSetStrings(l []string) map[string]bool {
	m := make(map[string]bool, len(l))
	for _, s := range l {
		m[s] = true
	}
	return m
}

// Merge combines the bundled table with a live list (merge_catalog). Live
// metadata overrides the bundled entry (reasoning merged field by field); the
// result follows the picker order. With extras, live models the curated order
// has never heard of are appended, but only when they carry a credit rate.
func Merge(live []Entry, realm string, extras bool) []Entry {
	var static []Entry
	if realm == "all" {
		static = append(append(static, Static("intl")...), Static("cn")...)
	} else {
		static = Static(realm)
	}
	primaryIDs := idSet(live)
	f2Shadowed := primaryIDs["hy4-preview"] && !primaryIDs["hy4-preview-f"]

	merged := map[string]*jsonx.Obj{}
	for _, item := range static {
		if item.ID == "hy4-preview-f" && f2Shadowed {
			continue
		}
		if IsChatModel(item.ID) {
			if _, dup := merged[item.ID]; !dup {
				merged[item.ID] = item.Meta.Clone()
			}
		}
	}
	for _, e := range live {
		if !IsChatModel(e.ID) {
			continue
		}
		if e.Meta != nil && e.Meta.Len() > 0 {
			base := merged[e.ID]
			if base == nil {
				base = jsonx.NewObj()
			}
			baseReasoning, _ := base.Obj("reasoning")
			e.Meta.Each(func(k string, v any) { base.Set(k, v) })
			if lr, ok := e.Meta.Obj("reasoning"); ok {
				base.Set("reasoning", MergeReasoning(baseReasoning, lr))
			}
			merged[e.ID] = base
		} else if merged[e.ID] == nil {
			merged[e.ID] = jsonx.NewObj()
		}
	}

	order := uiOrder(realm)
	var out []Entry
	seen := map[string]bool{}
	for _, mid := range order {
		if len(primaryIDs) > 0 && mid == "hy4-preview-f" && f2Shadowed {
			mid = "hy4-preview"
		}
		if m, ok := merged[mid]; ok && !seen[mid] {
			seen[mid] = true
			out = append(out, Entry{ID: mid, Meta: m})
		}
	}
	if extras {
		for _, e := range live {
			if m, ok := merged[e.ID]; ok && !seen[e.ID] {
				// models without a configured credit rate stay out of the public list
				if !jsonx.Truthy(m.Get("credits")) {
					continue
				}
				seen[e.ID] = true
				out = append(out, Entry{ID: e.ID, Meta: m})
			}
		}
	}
	return out
}

// ------------------------------------------------------------ remote config

// Parsed is the result of parsing a /v3/config response.
type Parsed struct {
	IDs  []string
	Meta map[string]*jsonx.Obj
}

func agentModelLists(payload *jsonx.Obj) (cli, other [][]string) {
	roots := []*jsonx.Obj{payload}
	if d, ok := payload.Obj("data"); ok {
		roots = append(roots, d)
	}
	for _, root := range roots {
		type named struct {
			name  string
			entry any
		}
		var entries []named
		switch ag := root.Get("agents").(type) {
		case *jsonx.Obj:
			ag.Each(func(k string, v any) { entries = append(entries, named{k, v}) })
		case []any:
			for _, e := range ag {
				name := ""
				if o, ok := e.(*jsonx.Obj); ok {
					name, _ = o.Str("name")
				}
				entries = append(entries, named{name, e})
			}
		default:
			continue
		}
		for _, n := range entries {
			o, ok := n.entry.(*jsonx.Obj)
			if !ok {
				continue
			}
			models, _ := o.Arr("models")
			if len(models) == 0 {
				continue
			}
			if _, isStr := models[0].(string); !isStr {
				continue
			}
			var ids []string
			for _, m := range models {
				if s, ok := m.(string); ok {
					if s = strings.TrimSpace(s); s != "" {
						ids = append(ids, s)
					}
				}
			}
			if len(ids) == 0 {
				continue
			}
			if strings.ToLower(strings.TrimSpace(n.name)) == "cli" {
				cli = append(cli, ids)
			} else {
				other = append(other, ids)
			}
		}
	}
	return cli, other
}

// ParseRemote reads a /v3/config response (or the desktop cache file, which is
// the same document without the "data" envelope). It returns nil when the
// document carries no model list: an unusable credential answers HTTP 200 with
// an empty catalogue and that must not be published as "no models".
func ParseRemote(payload any) *Parsed {
	root, ok := payload.(*jsonx.Obj)
	if !ok {
		return nil
	}
	cli, other := agentModelLists(root)
	pool := cli
	if len(pool) == 0 {
		pool = other
	}
	var best []string
	for _, l := range pool {
		if len(l) > len(best) { // max() keeps the first of equal lengths
			best = l
		}
	}
	var ids []string
	seen := map[string]bool{}
	for _, id := range best {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	meta := map[string]*jsonx.Obj{}
	var walk func(any)
	walk = func(n any) {
		switch t := n.(type) {
		case *jsonx.Obj:
			if models, _ := t.Arr("models"); len(models) > 0 {
				if first, ok := models[0].(*jsonx.Obj); ok && jsonx.Truthy(first.Get("id")) {
					for _, it := range models {
						item, ok := it.(*jsonx.Obj)
						if !ok {
							continue
						}
						id := strings.TrimSpace(jsonx.PyStrIfTruthy(item.Get("id")))
						if id != "" {
							if _, have := meta[id]; !have {
								meta[id] = item
							}
						}
					}
				}
			}
			t.Each(func(_ string, v any) { walk(v) })
		case []any:
			for _, v := range t {
				walk(v)
			}
		}
	}
	walk(payload)
	return &Parsed{IDs: ids, Meta: meta}
}

func isFree(credits string) bool {
	switch credits {
	case "x0.00", "x0", "0", "0.00":
		return true
	}
	return false
}

func creditsOf(o *jsonx.Obj) string {
	return strings.ToLower(strings.TrimSpace(jsonx.PyStrIfTruthy(o.Get("credits"))))
}

// Curate trims a remote list to what the picker should offer: no virtual
// aliases, no regional (-sg) or experimental (-x) variants, and when a free
// ("x0.00") sibling exists the paid sibling is dropped. Upstream order is kept.
func Curate(ids []string, meta map[string]*jsonx.Obj) []string {
	credits := map[string]string{}
	for _, src := range [][]Entry{Static("intl"), Static("cn")} {
		for _, e := range src {
			if _, ok := credits[e.ID]; !ok {
				credits[e.ID] = creditsOf(e.Meta)
			}
		}
	}
	for id, m := range meta {
		if m != nil {
			credits[id] = creditsOf(m)
		}
	}
	free := func(id string) bool { return isFree(credits[id]) }
	inList := idSetStrings(ids)

	var out []string
	for _, id := range ids {
		if !IsChatModel(id) {
			continue
		}
		if strings.HasSuffix(id, "-sg") || strings.HasSuffix(id, "-x") {
			continue
		}
		if strings.HasSuffix(id, "-f") {
			base := id[:len(id)-2]
			if inList[base] && free(base) && !free(id) {
				continue
			}
		} else if inList[id+"-f"] && free(id+"-f") && !free(id) {
			continue
		}
		out = append(out, id)
	}
	return out
}

// Live turns curated ids plus their metadata into merge input.
func Live(ids []string, meta map[string]*jsonx.Obj) []Entry {
	out := make([]Entry, 0, len(ids))
	for _, id := range ids {
		m := meta[id]
		if m == nil {
			m = jsonx.NewObj()
		}
		out = append(out, Entry{ID: id, Meta: m})
	}
	return out
}
