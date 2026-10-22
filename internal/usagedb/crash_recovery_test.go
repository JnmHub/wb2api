package usagedb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

const crashRecoveryEvents = 17

// Every event has independent dimensions/amounts: missing and duplicated events
// cannot cancel each other out in a count-only or aggregate-only assertion.
func crashRecoveryEvent(i int) Event {
	n := int64(i + 1)
	return Event{
		At:    time.Date(2026, 10, 20, 23, 0, 0, 0, time.UTC).Add(time.Duration(i) * 3 * time.Hour),
		Model: fmt.Sprintf("crash-model-%02d", i), Account: "crash-account", Realm: "intl", KeyID: "crash-key",
		Outcome: "completed", Status: 200, Prompt: n * 2, Completion: n * 3, Reasoning: n, Cached: n,
		Total: n * 5, Credit: float64(n) / 8, Error: fmt.Sprintf("detail-%02d", i),
	}
}

func crashRecoveryDetail(position int) bool {
	// QueueSize=8, sample at >=75% with SampleEvery=2: positions0..5 kept,
	// position6 omitted, position7 kept; >=8 shed. Concurrent Record ordering
	// is deliberately unconstrained; sampling follows the durable sequence.
	return position < 6 || position == 7
}

// Check sampling independently against durable sequence, and capture the exact
// model identities retained. Concurrent arrival order must not weaken the
// post-crash assertion to a mere detail count.
func crashRecoveryRetained(t *testing.T, h *sql.DB) map[string]bool {
	t.Helper()
	rows, err := h.Query("SELECT seq,payload,detail FROM events ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	retained := make(map[string]bool)
	seen := make(map[string]bool)
	position := 0
	for rows.Next() {
		var seq int
		var payload []byte
		var detail int
		if err = rows.Scan(&seq, &payload, &detail); err != nil {
			t.Fatal(err)
		}
		var e durableEvent
		if err = json.Unmarshal(payload, &e); err != nil {
			t.Fatal(err)
		}
		wantDetail := crashRecoveryDetail(position)
		if seq != position+1 || (detail != 0) != wantDetail || detail < 0 || detail > 1 || seen[e.Event.Model] {
			t.Fatalf("outbox sequence/sample/identity invalid: position=%d seq=%d detail=%d model=%q", position, seq, detail, e.Event.Model)
		}
		seen[e.Event.Model] = true
		if wantDetail {
			retained[e.Event.Model] = true
		}
		position++
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if position != crashRecoveryEvents || len(retained) != 7 {
		t.Fatalf("outbox identities=%d retained=%d", position, len(retained))
	}
	for i := range crashRecoveryEvents {
		if !seen[crashRecoveryEvent(i).Model] {
			t.Fatalf("outbox missing accepted model %q", crashRecoveryEvent(i).Model)
		}
	}
	return retained
}

type crashRecoveryHandshake struct {
	Source   string
	Count    int
	Phase    string
	Retained map[string]bool
}

// This helper deliberately does not Close or unlock anything. The controlling
// test must kill this actual process after the durable-admission handshake.
func TestUsageCrashRecoveryProcessHelper(t *testing.T) {
	phase := os.Getenv("WB_USAGE_CRASH_HELPER_PHASE")
	if phase == "" {
		return
	}
	if phase != "pending" && phase != "committed-unacked" {
		t.Fatal("invalid helper phase")
	}
	d, err := OpenWithOptions(os.Getenv("WB_USAGE_CRASH_HELPER_PATH"), Options{
		QueueSize: 8, Overflow: OverflowSample, SampleEvery: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A real sink transaction failure prevents periodic delivery throughout
	// Record admission and sampling. No production hooks or forged receipt.
	if _, err = d.sql.Exec("CREATE TRIGGER crash_block_sink BEFORE INSERT ON usage_facts BEGIN SELECT RAISE(ABORT,'crash-test sink blocked'); END"); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	accepted := make(chan error, crashRecoveryEvents)
	for i := range crashRecoveryEvents {
		go func() {
			<-start
			if err := d.Record(crashRecoveryEvent(i)); err != nil {
				accepted <- fmt.Errorf("Record(%d) was not accepted: %w", i, err)
				return
			}
			accepted <- nil
		}()
	}
	close(start)
	for range crashRecoveryEvents {
		if err = <-accepted; err != nil {
			t.Fatal(err)
		}
	}
	retained := crashRecoveryRetained(t, d.outbox)
	if phase == "committed-unacked" {
		// Commit through the normal writer, but abort its separate local delete.
		// This is the crash-after-sink-commit/before-local-ack window, not a
		// fabricated usage_sources row or manual invocation of writeBatch.
		if _, err = d.outbox.Exec("CREATE TRIGGER crash_block_ack BEFORE DELETE ON events BEGIN SELECT RAISE(ABORT,'crash-test ack blocked'); END"); err != nil {
			t.Fatal(err)
		}
		if _, err = d.sql.Exec("DROP TRIGGER crash_block_sink"); err != nil {
			t.Fatal(err)
		}
		if err = d.Flush(); err == nil {
			t.Fatal("injected acknowledgment failure unexpectedly succeeded")
		}
		assertCrashRecoverySink(t, d.sql, crashRecoveryEvents, retained)
	} else {
		if err = d.Flush(); err == nil {
			t.Fatal("injected sink failure unexpectedly succeeded")
		}
		assertCrashRecoverySink(t, d.sql, 0, retained)
	}
	var count int
	if err = d.outbox.QueryRow("SELECT COUNT(*) FROM events").Scan(&count); err != nil || count != crashRecoveryEvents {
		t.Fatalf("durable rows=%d err=%v", count, err)
	}
	if err = json.NewEncoder(os.Stdout).Encode(crashRecoveryHandshake{Source: d.source, Count: count, Phase: phase, Retained: retained}); err != nil {
		t.Fatal(err)
	}
	// The live dispatcher keeps running; only Process.Kill may end this helper.
	select {}
}

// This exercises actual abrupt process death with OS ownership/WAL recovery.
// It does not emulate power loss, damaged storage, or dishonest hardware fsync.
func TestUsageCrashRecoveryKillAndRestart(t *testing.T) {
	for _, phase := range []string{"pending", "committed-unacked"} {
		t.Run(phase, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "usage with spaces.db")
			handshake := killCrashRecoveryProcess(t, path, phase)
			if handshake.Phase != phase || handshake.Count != crashRecoveryEvents || handshake.Source == "" || len(handshake.Retained) != 7 {
				t.Fatalf("bad handshake: %+v", handshake)
			}
			// Inspect the real on-disk state before recovery can consume it.
			sink := openCrashRecoverySQL(t, path)
			wantBefore := 0
			if phase == "committed-unacked" {
				wantBefore = crashRecoveryEvents
			}
			assertCrashRecoverySink(t, sink, wantBefore, handshake.Retained)
			assertCrashRecoveryReceipt(t, sink, handshake.Source, wantBefore)
			if _, err := sink.Exec("DROP TRIGGER IF EXISTS crash_block_sink"); err != nil {
				t.Fatal(err)
			}
			if err := sink.Close(); err != nil {
				t.Fatal(err)
			}
			outbox := openCrashRecoverySQL(t, path+".outbox.db")
			var count, details, seq int
			if err := outbox.QueryRow("SELECT COUNT(*),SUM(detail),MAX(seq) FROM events").Scan(&count, &details, &seq); err != nil || count != crashRecoveryEvents || details != 7 || seq != crashRecoveryEvents {
				t.Fatalf("surviving outbox count/details/seq=%d/%d/%d err=%v", count, details, seq, err)
			}
			if retained := crashRecoveryRetained(t, outbox); !reflect.DeepEqual(retained, handshake.Retained) {
				t.Fatalf("sampling identities changed across kill: %v != %v", retained, handshake.Retained)
			}
			if _, err := outbox.Exec("DROP TRIGGER IF EXISTS crash_block_ack"); err != nil {
				t.Fatal(err)
			}
			if err := outbox.Close(); err != nil {
				t.Fatal(err)
			}
			for cycle := range 3 {
				func() {
					// Reopening without removing the retained lock file verifies the
					// OS released ownership after kill, and keeps source identity.
					d, err := Open(path, 32)
					if err != nil {
						t.Fatalf("reopen cycle%d: %v", cycle, err)
					}
					defer d.Close()
					if d.source != handshake.Source {
						t.Fatalf("source changed across crash: %q != %q", d.source, handshake.Source)
					}
					if err = d.Flush(); err != nil {
						t.Fatal(err)
					}
					n := crashRecoveryEvents
					if cycle > 0 {
						n++
					}
					assertCrashRecoverySink(t, d.sql, n, handshake.Retained)
					assertCrashRecoveryReceipt(t, d.sql, handshake.Source, n)
					if cycle == 0 {
						// Empty-queue sequence reuse would be silently discarded by the
						// surviving high-water receipt. Prove the next Record is new.
						if err = d.Record(crashRecoveryEvent(crashRecoveryEvents)); err != nil {
							t.Fatal(err)
						}
						if err = d.Flush(); err != nil {
							t.Fatal(err)
						}
						n++
					}
					if err = d.RebuildRollups(context.Background()); err != nil {
						t.Fatal(err)
					}
					assertCrashRecoverySink(t, d.sql, n, handshake.Retained)
					assertCrashRecoveryReceipt(t, d.sql, handshake.Source, n)
					var pending int
					if err = d.outbox.QueryRow("SELECT COUNT(*) FROM events").Scan(&pending); err != nil || pending != 0 || d.Health().PendingEvents != 0 {
						t.Fatalf("undrained replay rows=%d health=%+v err=%v", pending, d.Health(), err)
					}
					if err = d.Close(); err != nil {
						t.Fatal(err)
					}
				}()
			}
		})
	}
}

func killCrashRecoveryProcess(t *testing.T, path, phase string) crashRecoveryHandshake {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestUsageCrashRecoveryProcessHelper$", "-test.timeout=45s")
	cmd.Env = append(os.Environ(), "WB_USAGE_CRASH_HELPER_PHASE="+phase, "WB_USAGE_CRASH_HELPER_PATH="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	// File logging avoids an unbounded output buffer and concurrent reads of
	// helper diagnostics while Wait is still writing to an in-memory buffer.
	log, err := os.CreateTemp(t.TempDir(), "crash-helper-stderr-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	type result struct {
		handshake crashRecoveryHandshake
		err       error
	}
	ready := make(chan result, 1)
	go func() {
		var r result
		r.err = json.NewDecoder(stdout).Decode(&r.handshake)
		ready <- r
	}()
	var r result
	select {
	case r = <-ready:
	case <-time.After(35 * time.Second):
		// Kill/reap before failing, which also unblocks the decoder goroutine.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		waited = true
		<-ready
		diagnostics, _ := os.ReadFile(log.Name())
		t.Fatalf("helper handshake timeout: %s", diagnostics)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatalf("failed to abruptly kill live helper: %v", err)
	}
	err = cmd.Wait()
	waited = true
	if err == nil || cmd.ProcessState == nil || cmd.ProcessState.Success() {
		t.Fatalf("helper did not terminate unsuccessfully from kill: %v", err)
	}
	if r.err != nil {
		diagnostics, _ := os.ReadFile(log.Name())
		t.Fatalf("helper handshake: %v; stderr: %s", r.err, diagnostics)
	}
	return r.handshake
}

func openCrashRecoverySQL(t *testing.T, path string) *sql.DB {
	t.Helper()
	h, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func assertCrashRecoveryReceipt(t *testing.T, h *sql.DB, source string, seq int) {
	t.Helper()
	var count int
	if err := h.QueryRow("SELECT COUNT(*) FROM usage_sources").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if seq == 0 {
		if count != 0 {
			t.Fatalf("uncommitted sink acquired receipt: %d", count)
		}
		return
	}
	var got int
	if err := h.QueryRow("SELECT seq FROM usage_sources WHERE id=?", source).Scan(&got); err != nil || count != 1 || got != seq {
		t.Fatalf("receipt sources=%d seq=%d want=%d err=%v", count, got, seq, err)
	}
}

func assertCrashRecoverySink(t *testing.T, h *sql.DB, n int, retained map[string]bool) {
	t.Helper()
	for _, table := range []string{"usage_facts", "daily", "hourly", "requests"} {
		wantCount := n
		if table == "requests" {
			wantCount = 0
			for i := range n {
				if retained[crashRecoveryEvent(i).Model] || i >= crashRecoveryEvents {
					wantCount++
				}
			}
		}
		var count int
		if err := h.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != wantCount {
			t.Fatalf("%s rows=%d want=%d err=%v", table, count, wantCount, err)
		}
		for i := range n {
			e := crashRecoveryEvent(i)
			var gotCount int
			if err := h.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE model=?", e.Model).Scan(&gotCount); err != nil {
				t.Fatal(err)
			}
			if table == "requests" && !retained[e.Model] && i < crashRecoveryEvents {
				if gotCount != 0 {
					t.Fatalf("sampled detail resurrected for %s", e.Model)
				}
				continue
			}
			if gotCount != 1 {
				t.Fatalf("%s model=%s missing/duplicate rows=%d", table, e.Model, gotCount)
			}
			var total, prompt, completion, reasoning, cached int64
			var credit float64
			if err := h.QueryRow("SELECT total,prompt,completion,reasoning,cached,credit FROM "+table+" WHERE model=?", e.Model).Scan(&total, &prompt, &completion, &reasoning, &cached, &credit); err != nil || total != e.Total || prompt != e.Prompt || completion != e.Completion || reasoning != e.Reasoning || cached != e.Cached || credit != e.Credit {
				t.Fatalf("%s model=%s accounting=%d/%d/%d/%d/%d/%v err=%v", table, e.Model, total, prompt, completion, reasoning, cached, credit, err)
			}
			if table == "daily" || table == "hourly" {
				var requests int
				if err := h.QueryRow("SELECT requests FROM "+table+" WHERE model=?", e.Model).Scan(&requests); err != nil || requests != 1 {
					t.Fatalf("%s model=%s requests=%d err=%v", table, e.Model, requests, err)
				}
			}
			if table == "requests" {
				var detail string
				if err := h.QueryRow("SELECT error FROM requests WHERE model=?", e.Model).Scan(&detail); err != nil || detail != e.Error {
					t.Fatalf("retained detail model=%s value=%q err=%v", e.Model, detail, err)
				}
			}
		}
	}
}
