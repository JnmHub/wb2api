package logbuf

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestRingBoundedAndFiltered(t *testing.T) {
	r := New(10)
	for i := 0; i < 25; i++ {
		lvl := "INFO"
		if i%5 == 0 {
			lvl = "ERROR"
		}
		r.Add(lvl, "", "account number "+string(rune('a'+i%26)), timeNow())
	}
	all, last := r.Select(Query{Limit: 100})
	if len(all) != 10 || last != 25 {
		t.Fatalf("ring keeps the newest 10: %d last=%d", len(all), last)
	}
	errs, _ := r.Select(Query{Level: "ERROR"})
	for _, e := range errs {
		if e.Level != "ERROR" {
			t.Errorf("level filter: %+v", e)
		}
	}
	tail, _ := r.Select(Query{After: 23})
	if len(tail) != 2 || tail[0].ID != 24 {
		t.Fatalf("tail after id: %+v", tail)
	}
	if got, _ := r.Select(Query{Text: "NUMBER Z"}); len(got) != 0 && !strings.Contains(strings.ToLower(got[0].Msg), "number z") {
		t.Errorf("text filter: %v", got)
	}
	if all[0].Tag != "accounts" {
		t.Errorf("tag inferred from text: %q", all[0].Tag)
	}
	r.Clear()
	if got, _ := r.Select(Query{}); len(got) != 0 {
		t.Error("clear")
	}
	if e := r.Add("INFO", "", "x", timeNow()); e.ID != 26 {
		t.Errorf("ids keep counting after a clear: %d", e.ID)
	}
}

func TestSlogBridgeKeepsForwarding(t *testing.T) {
	ring := New(50)
	var out bytes.Buffer
	log := slog.New(NewHandler(ring, slog.NewTextHandler(&out, nil)))
	log.Info("upstream hiccup", "account", "abcd1234", "tag", "egress")
	log.Warn("careful")
	log.Error("boom", "err", "x")
	log.Debug("hidden")
	got, _ := ring.Select(Query{})
	if len(got) != 3 || got[0].Tag != "egress" || strings.Contains(got[0].Msg, "tag=") || !strings.Contains(got[0].Msg, "account=abcd1234") ||
		got[1].Level != "WARN" || got[2].Level != "ERROR" {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(out.String(), "upstream hiccup") {
		t.Errorf("still forwarded: %q", out.String())
	}
}
