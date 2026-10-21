package redisx

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"wb2api/internal/sched"
)

func TestStatsStorePublishesPerReplicaAndMerges(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := Open(context.Background(), mr.Addr(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	st := NewStatsStore(c.Client)
	now := time.Now()
	if err := st.Publish(ctx, "replica-a", map[string]sched.SharedSample{
		"acct": {TTFT: 100, Err: 0.1, Seen: now, HasTTFT: true, Samples: 2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Publish(ctx, "replica-b", map[string]sched.SharedSample{
		"acct": {TTFT: 500, Err: 0.6, Seen: now.Add(time.Second), HasTTFT: true, Samples: 3},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.Load(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if sample := got["acct"]; sample.TTFT != 500 || sample.Err != 0.6 {
		t.Fatalf("merged = %+v, want worst values", sample)
	}
	if got, err = st.Load(ctx, "replica-a"); err != nil {
		t.Fatal(err)
	}
	if sample := got["acct"]; sample.TTFT != 500 {
		t.Fatalf("exclude own snapshot = %+v", sample)
	}

	if err := st.Publish(ctx, "replica-a", nil); err != nil {
		t.Fatal(err)
	}
	if got, err = st.Load(ctx, "replica-a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["acct"]; !ok {
		t.Fatal("replica-b snapshot should survive replica-a deletion")
	}
	mr.FastForward(31 * time.Second)
	if got, err = st.Load(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expired snapshots still visible: %+v", got)
	}
}
