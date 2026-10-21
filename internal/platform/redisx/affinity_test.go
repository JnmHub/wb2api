package redisx

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestAffinityStoreSharesAndTrimsBindings(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := Open(context.Background(), mr.Addr(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	st := NewAffinityStore(c.Client)
	for _, uid := range []string{"acct-0", "acct-1", "acct-2", "acct-3"} {
		if err := st.Bind(ctx, "sess", uid, time.Hour, 3); err != nil {
			t.Fatalf("bind %s: %v", uid, err)
		}
	}
	got, err := st.Recent(ctx, "sess", 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"acct-3", "acct-2", "acct-1"}
	if len(got) != len(want) {
		t.Fatalf("recent = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("recent = %v, want %v", got, want)
		}
	}
	if err := st.Demote(ctx, "sess", "acct-2"); err != nil {
		t.Fatal(err)
	}
	got, err = st.Recent(ctx, "sess", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "acct-3" || got[1] != "acct-1" {
		t.Fatalf("recent after demote = %v", got)
	}
}

func TestAffinityStoreHashesSessionKey(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := Open(context.Background(), mr.Addr(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	st := NewAffinityStore(c.Client)
	if err := st.Bind(context.Background(), "customer-secret-session", "acct", time.Hour, 3); err != nil {
		t.Fatal(err)
	}
	for _, key := range mr.Keys() {
		if key == "customer-secret-session" {
			t.Fatal("raw session key stored in Redis")
		}
	}
}

func TestAffinityStoreExpiresEntries(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := Open(context.Background(), mr.Addr(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	st := NewAffinityStore(c.Client)
	if err := st.Bind(context.Background(), "sess", "acct", 40*time.Millisecond, 3); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	got, err := st.Recent(context.Background(), "sess", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expired binding returned: %v", got)
	}
}
