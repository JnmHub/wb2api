package shape

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"wb2api/internal/jsonx"
)

type mapCatalog map[string]struct {
	Max    int    `json:"max"`
	Effort string `json:"effort"`
}

func (m mapCatalog) MaxOutputTokens(model string) int  { return m[model].Max }
func (m mapCatalog) DefaultEffort(model string) string { return m[model].Effort }

func loadCatalog(t *testing.T) mapCatalog {
	t.Helper()
	raw, err := os.ReadFile("testdata/catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	var c mapCatalog
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestBuildGolden feeds every payload to Build and requires the compact JSON to
// be byte-identical to what the legacy build_upstream_body + encode produced.
func TestBuildGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/build.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs []struct {
		Payload string `json:"payload"`
		Opts    struct {
			Mode     string `json:"mode"`
			Text     string `json:"text"`
			Degraded bool   `json:"degraded"`
			Preserve bool   `json:"preserve"`
		} `json:"opts"`
		Out string `json:"out"`
	}
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	if len(vs) < 300 {
		t.Fatalf("too few vectors: %d", len(vs))
	}
	cat := loadCatalog(t)
	failures := 0
	for i, v := range vs {
		payload, err := jsonx.UnmarshalObj([]byte(v.Payload))
		if err != nil {
			t.Fatalf("#%d bad payload: %v", i, err)
		}
		before, _ := jsonx.Marshal(payload)
		body := Build(payload, Options{
			PromptMode: v.Opts.Mode, PromptText: v.Opts.Text, Degraded: v.Opts.Degraded,
			PreserveContract: v.Opts.Preserve, Catalog: cat,
		})
		got, err := jsonx.Marshal(body)
		if err != nil {
			t.Fatalf("#%d marshal: %v", i, err)
		}
		if string(got) != v.Out {
			failures++
			if failures <= 8 {
				t.Errorf("#%d differs\npayload: %s\n got:  %s\n want: %s", i, v.Payload, got, v.Out)
			}
		}
		after, _ := jsonx.Marshal(payload)
		if string(before) != string(after) {
			t.Errorf("#%d: Build modified its input\nbefore: %s\nafter:  %s", i, before, after)
		}
	}
	if failures > 0 {
		t.Fatalf("%d of %d vectors differ", failures, len(vs))
	}
}

func TestJSONRoundTripKeepsOrderAndEscapes(t *testing.T) {
	in := `{"z":1,"a":{"y":[1,2.50,"x"],"b":null},"m":"é\n\u0001 </s> &  ","dup":1,"dup":2}`
	o, err := jsonx.UnmarshalObj([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := jsonx.Marshal(o)
	want := "{\"z\":1,\"a\":{\"y\":[1,2.50,\"x\"],\"b\":null},\"m\":\"é\\n\\u0001 </s> &  \",\"dup\":2}"
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if _, err := jsonx.UnmarshalObj([]byte(`{"a":1} x`)); err == nil {
		t.Fatal("trailing garbage must be rejected")
	}
	if _, err := jsonx.UnmarshalObj([]byte(`[1]`)); err == nil {
		t.Fatal("non-object must be rejected")
	}
}

func TestObjSemantics(t *testing.T) {
	o := jsonx.NewObj()
	o.Set("a", 1)
	o.Set("b", 2)
	o.Set("a", 9) // keeps position
	o.Delete("b")
	o.Set("b", 3) // moves to the end
	if strings.Join(o.Keys(), ",") != "a,b" {
		t.Fatalf("keys %v", o.Keys())
	}
	c := o.Clone()
	c.Set("a", 0)
	if o.Get("a") != 9 {
		t.Fatal("Clone must not share the key table")
	}
}
