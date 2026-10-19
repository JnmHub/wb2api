package openai

import "testing"

// fixedCatalog supplies a model pinned to one reasoning level, like the real
// catalogue does for models with reasoning.effort but no supportedEfforts.
type fixedCatalog struct{ fixed, def string }

func (c fixedCatalog) MaxOutputTokens(string) int  { return 0 }
func (c fixedCatalog) FixedEffort(string) string   { return c.fixed }
func (c fixedCatalog) DefaultEffort(string) string { return c.def }

func TestUsageRecordsTheCataloguePinnedEffort(t *testing.T) {
	e := newEnv(t, sseUpstream("hi"), 1, "")
	e.h.Catalog = fixedCatalog{fixed: "high"}
	e.post(t, chatBody, nil)
	ev := e.event(t)
	if ev.Effort != "high" {
		t.Fatalf("a pinned model must report its fixed effort, got %q", ev.Effort)
	}
}

func TestUsageRecordsTheModelDefaultEffort(t *testing.T) {
	e := newEnv(t, sseUpstream("hi"), 1, "")
	e.h.Catalog = fixedCatalog{def: "medium"}
	e.post(t, chatBody, nil)
	ev := e.event(t)
	if ev.Effort != "medium" {
		t.Fatalf("a plain request must fall back to the model default, got %q", ev.Effort)
	}
}

func TestThinkingDisabledReportsNoEffort(t *testing.T) {
	e := newEnv(t, sseUpstream("hi"), 1, "")
	e.h.Catalog = fixedCatalog{def: "medium"}
	body := `{"model":"glm-5.3","stream":true,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`
	e.post(t, body, nil)
	ev := e.event(t)
	if ev.Effort != "none" {
		t.Fatalf("thinking disabled must report none, got %q", ev.Effort)
	}
}
