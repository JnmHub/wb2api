package catalog

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"wb2api/internal/jsonx"
)

// The vectors come from the legacy Python implementation
// (py -I tools/genvectors/gen_catalog.py); every comparison is on compact
// JSON text, so key order and number spelling are part of the contract.

type vectors struct {
	Clamps map[string]int64 `json:"clamps"`
	IsChat []struct {
		ID   string `json:"id"`
		Want bool   `json:"want"`
	} `json:"is_chat"`
	Parse map[string]struct {
		Payload json.RawMessage `json:"payload"`
		Want    *struct {
			IDs  []string        `json:"ids"`
			Meta json.RawMessage `json:"meta"`
		} `json:"want"`
	} `json:"parse"`
	Curate []struct {
		Realm string          `json:"realm"`
		IDs   []string        `json:"ids"`
		Meta  json.RawMessage `json:"meta"`
		Want  []string        `json:"want"`
	} `json:"curate"`
	Merge []struct {
		Realm  string `json:"realm"`
		Extras bool   `json:"extras"`
		Live   []struct {
			ID   string          `json:"id"`
			Meta json.RawMessage `json:"meta"`
		} `json:"live"`
		Want []struct {
			ID   string          `json:"id"`
			Meta json.RawMessage `json:"meta"`
		} `json:"want"`
	} `json:"merge"`
	Entry []struct {
		ID   string          `json:"id"`
		Meta json.RawMessage `json:"meta"`
		Want string          `json:"want"`
	} `json:"entry"`
	Limits struct {
		Index map[string][2]int64 `json:"index"`
		Cases []struct {
			ID   string   `json:"id"`
			RC   any      `json:"rc"`
			RO   any      `json:"ro"`
			Want [2]int64 `json:"want"`
		} `json:"cases"`
	} `json:"limits"`
	ModelsDev struct {
		Doc  json.RawMessage     `json:"doc"`
		Want map[string][2]int64 `json:"want"`
	} `json:"modelsdev"`
	Defaults []struct {
		ID        string `json:"id"`
		MaxTokens int    `json:"max_tokens"`
		Effort    string `json:"effort"`
	} `json:"defaults"`
}

func load(t *testing.T) *vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return &v
}

func parseObj(t *testing.T, raw []byte) *jsonx.Obj {
	t.Helper()
	o, err := jsonx.UnmarshalObj(raw)
	if err != nil {
		t.Fatalf("%v in %s", err, raw)
	}
	return o
}

func enc(t *testing.T, v any) string {
	t.Helper()
	b, err := jsonx.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBundledTables(t *testing.T) {
	if len(Static("intl")) != 24 || len(Static("cn")) != 39 {
		t.Fatalf("bundled tables: intl=%d cn=%d", len(Static("intl")), len(Static("cn")))
	}
}

func TestIsChatModel(t *testing.T) {
	for _, c := range load(t).IsChat {
		if got := IsChatModel(c.ID); got != c.Want {
			t.Errorf("IsChatModel(%q) = %v, want %v", c.ID, got, c.Want)
		}
	}
}

func TestParseRemote(t *testing.T) {
	for name, c := range load(t).Parse {
		doc, err := jsonx.Unmarshal(c.Payload)
		if err != nil {
			t.Fatal(err)
		}
		got := ParseRemote(doc)
		switch {
		case c.Want == nil && got != nil:
			t.Errorf("%s: want nil, got %v", name, got.IDs)
		case c.Want != nil && got == nil:
			t.Errorf("%s: want %v, got nil", name, c.Want.IDs)
		case c.Want != nil:
			if strings.Join(got.IDs, "|") != strings.Join(c.Want.IDs, "|") {
				t.Errorf("%s ids:\n got %v\nwant %v", name, got.IDs, c.Want.IDs)
			}
			// meta is a map in Python; compare by id
			wantMeta := parseObj(t, c.Want.Meta)
			if len(got.Meta) != wantMeta.Len() {
				t.Errorf("%s: %d meta entries, want %d", name, len(got.Meta), wantMeta.Len())
			}
			wantMeta.Each(func(id string, w any) {
				if g := got.Meta[id]; g == nil || enc(t, g) != enc(t, w) {
					t.Errorf("%s meta[%s] differs", name, id)
				}
			})
		}
	}
}

func metaMap(t *testing.T, raw json.RawMessage) map[string]*jsonx.Obj {
	t.Helper()
	m := map[string]*jsonx.Obj{}
	parseObj(t, raw).Each(func(k string, v any) { m[k], _ = v.(*jsonx.Obj) })
	return m
}

func TestCurate(t *testing.T) {
	for i, c := range load(t).Curate {
		got := Curate(c.IDs, metaMap(t, c.Meta))
		if strings.Join(got, "|") != strings.Join(c.Want, "|") {
			t.Errorf("case %d (%s):\n got %v\nwant %v", i, c.Realm, got, c.Want)
		}
	}
}

func TestMerge(t *testing.T) {
	for i, c := range load(t).Merge {
		var live []Entry
		for _, l := range c.Live {
			live = append(live, Entry{ID: l.ID, Meta: parseObj(t, l.Meta)})
		}
		got := Merge(live, c.Realm, c.Extras)
		if len(got) != len(c.Want) {
			t.Errorf("case %d (%s extras=%v): %d entries, want %d", i, c.Realm, c.Extras, len(got), len(c.Want))
			continue
		}
		for j, w := range c.Want {
			if got[j].ID != w.ID {
				t.Errorf("case %d [%d]: id %s, want %s", i, j, got[j].ID, w.ID)
				continue
			}
			if g, want := enc(t, got[j].Meta), enc(t, parseObj(t, w.Meta)); g != want {
				t.Errorf("case %d [%s]: meta differs\n got %s\nwant %s", i, w.ID, g, want)
			}
		}
	}
}

func TestModelEntry(t *testing.T) {
	v := load(t)
	env := EntryEnv{Created: 0, Clamp: func(m string) int64 { return v.Clamps[m] }}
	for _, c := range v.Entry {
		got := enc(t, ModelEntry(c.ID, parseObj(t, c.Meta), env))
		if got != c.Want {
			t.Errorf("%s:\n got %s\nwant %s", c.ID, got, c.Want)
		}
	}
}

func TestLimits(t *testing.T) {
	v := load(t)
	index := v.Limits.Index
	for _, c := range v.Limits.Cases {
		rc := pyInt(toNumber(c.RC))
		ro := pyInt(toNumber(c.RO))
		ctx, out := Limits(c.ID, rc, ro, index)
		if ctx != c.Want[0] || out != c.Want[1] {
			t.Errorf("Limits(%q,%v,%v) = %d,%d want %v", c.ID, c.RC, c.RO, ctx, out, c.Want)
		}
	}
}

func toNumber(v any) any {
	if f, ok := v.(float64); ok {
		return json.Number(jsonxNum(f))
	}
	return v
}

func jsonxNum(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

func TestBuildIndex(t *testing.T) {
	v := load(t)
	var doc map[string]struct {
		Models map[string]struct {
			Limit struct {
				Context json.Number `json:"context"`
				Output  json.Number `json:"output"`
			} `json:"limit"`
		} `json:"models"`
	}
	// the vector doc has a provider whose value is a string; real documents are
	// tolerated the same way by decoding provider by provider
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(v.ModelsDev.Doc, &raw); err != nil {
		t.Fatal(err)
	}
	doc = map[string]struct {
		Models map[string]struct {
			Limit struct {
				Context json.Number `json:"context"`
				Output  json.Number `json:"output"`
			} `json:"limit"`
		} `json:"models"`
	}{}
	for k, r := range raw {
		var p struct {
			Models map[string]struct {
				Limit struct {
					Context json.Number `json:"context"`
					Output  json.Number `json:"output"`
				} `json:"limit"`
			} `json:"models"`
		}
		if json.Unmarshal(r, &p) == nil {
			doc[k] = p
		}
	}
	got := buildIndex(doc)
	if len(got) != len(v.ModelsDev.Want) {
		t.Errorf("index size %d, want %d (%v)", len(got), len(v.ModelsDev.Want), got)
	}
	for k, w := range v.ModelsDev.Want {
		if got[k] != w {
			t.Errorf("index[%s] = %v, want %v", k, got[k], w)
		}
	}
}

func TestCatalogDefaults(t *testing.T) {
	s := &Service{}
	for _, c := range load(t).Defaults {
		if got := s.MaxOutputTokens(c.ID); got != c.MaxTokens {
			t.Errorf("MaxOutputTokens(%q) = %d, want %d", c.ID, got, c.MaxTokens)
		}
		if got := s.DefaultEffort(c.ID); got != c.Effort {
			t.Errorf("DefaultEffort(%q) = %q, want %q", c.ID, got, c.Effort)
		}
	}
}
