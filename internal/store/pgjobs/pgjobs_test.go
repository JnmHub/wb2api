package pgjobs

import (
	"context"
	"os"
	"testing"
	"time"

	"wb2api/internal/jobs"
	"wb2api/internal/platform/pg"
)

func TestPostgresJobsShareSnapshotsAndCancel(t *testing.T) {
	dsn := os.Getenv("WB_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("WB_TEST_POSTGRES_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `delete from wb_jobs`); err != nil {
		t.Fatal(err)
	}
	store := New(pool.Pool)
	j := jobs.Job{ID: "job-1", Kind: "refresh", State: jobs.Running, Total: 2, Owner: "replica-a", Started: time.Now().Unix()}
	if err := store.Save(ctx, j); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != j.ID || got[0].Owner != "replica-a" {
		t.Fatalf("loaded %+v", got)
	}
	ok, err := store.RequestCancel(ctx, j.ID)
	if err != nil || !ok {
		t.Fatalf("request cancel ok=%v err=%v", ok, err)
	}
	// An owner snapshot written after the request must not drop the flag.
	if err := store.Save(ctx, j); err != nil {
		t.Fatal(err)
	}
	got, err = store.Load(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].CancelRequested {
		t.Fatalf("cancel flag lost: %+v", got)
	}
	j.State = jobs.Done
	j.Finished = time.Now().Unix()
	if err := store.Save(ctx, j); err != nil {
		t.Fatal(err)
	}
	ok, err = store.RequestCancel(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("finished job accepted a cancel request")
	}
}
