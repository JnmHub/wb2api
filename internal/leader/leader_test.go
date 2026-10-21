package leader

import (
	"context"
	"os"
	"testing"
	"time"

	"wb2api/internal/platform/pg"
)

func TestOnlyOneReplicaLeads(t *testing.T) {
	dsn := os.Getenv("WB_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("WB_TEST_POSTGRES_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	ctxA, stopA := context.WithCancel(context.Background())
	ctxB, stopB := context.WithCancel(context.Background())
	defer stopA()
	defer stopB()
	a := New(pool.Pool, 0x7762326170692d74, nil)
	b := New(pool.Pool, 0x7762326170692d74, nil)
	go a.Run(ctxA)
	go b.Run(ctxB)

	deadline := time.Now().Add(6 * time.Second)
	for !(a.IsLeader() != b.IsLeader()) {
		if time.Now().After(deadline) {
			t.Fatalf("no single leader: a=%v b=%v", a.IsLeader(), b.IsLeader())
		}
		time.Sleep(50 * time.Millisecond)
	}
	var follower *Elector
	if a.IsLeader() {
		follower = b
		stopA()
	} else {
		follower = a
		stopB()
	}
	deadline = time.Now().Add(8 * time.Second)
	for !follower.IsLeader() {
		if time.Now().After(deadline) {
			t.Fatal("the surviving replica did not take over")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
