package accounts

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadStrictReportsDirectoryErrors(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	if recs, bad, err := LoadStrict(missing, time.Now()); !errors.Is(err, os.ErrNotExist) || len(recs) != 0 || len(bad) != 0 {
		t.Fatalf("missing directory: count=%d bad=%d err=%v", len(recs), len(bad), err)
	}
	notDir := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(notDir, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadStrict(notDir, time.Now()); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("non-directory should be a hard error: %v", err)
	}
	if recs, bad, err := LoadStrict(dir, time.Now()); err != nil || len(recs) != 0 || len(bad) != 0 {
		t.Fatalf("readable empty directory: count=%d bad=%d err=%v", len(recs), len(bad), err)
	}
	// Legacy standalone loading keeps its established tolerant behavior.
	if recs, bad := Load(missing, time.Now()); len(recs) != 0 || len(bad) != 0 {
		t.Fatalf("legacy loader changed: count=%d bad=%d", len(recs), len(bad))
	}
}
