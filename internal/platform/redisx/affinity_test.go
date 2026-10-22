package redisx

import (
	"context"
	"strings"
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

func TestAffinityStoreVersionFencesDemotionAndAllowsNewObservation(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := Open(context.Background(), mr.Addr(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	a, b := NewAffinityStore(c.Client), NewAffinityStore(c.Client)
	ctx := context.Background()
	_, old, err := a.RecentVersion(ctx, "sess", 3)
	if err != nil || old == "" {
		t.Fatalf("absent observation: %q, %v", old, err)
	}
	if err := b.Demote(ctx, "sess", "failed"); err != nil {
		t.Fatal(err)
	}
	if accepted, err := a.BindVersion(ctx, "sess", "failed", old, time.Hour, 3); err != nil || accepted {
		t.Fatalf("stale bind accepted=%v err=%v", accepted, err)
	}
	_, fresh, err := b.RecentVersion(ctx, "sess", 3)
	if err != nil || fresh == old {
		t.Fatalf("new generation: %q, %v", fresh, err)
	}
	if accepted, err := b.BindVersion(ctx, "sess", "failed", fresh, time.Hour, 3); err != nil || !accepted {
		t.Fatalf("new bind accepted=%v err=%v", accepted, err)
	}
	if accepted, err := a.BindVersion(ctx, "sess", "failed", old, time.Hour, 3); err != nil || accepted {
		t.Fatalf("late old bind accepted=%v err=%v", accepted, err)
	}
	got, err := a.Recent(ctx, "sess", 3)
	if err != nil || len(got) != 1 || got[0] != "failed" {
		t.Fatalf("fresh binding lost: %v, %v", got, err)
	}
}

func TestAffinityStoreObservationMaxAgeAndMetadataExpiryABA(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := Open(context.Background(), mr.Addr(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	st := NewAffinityStore(c.Client)
	ctx := context.Background()
	base := time.Unix(1700000000, 0)
	mr.SetTime(base)
	_, old, err := st.RecentVersion(ctx, "sess", 3)
	if err != nil {
		t.Fatal(err)
	}
	if AffinityMetadataTTL <= AffinityOperationMaxAge {
		t.Fatal("metadata retention must exceed maximum operation age")
	}
	meta := st.keys("sess")[1]
	if ttl := mr.TTL(meta); ttl != AffinityMetadataTTL {
		t.Fatalf("metadata TTL=%v", ttl)
	}
	mr.SetTime(base.Add(AffinityOperationMaxAge + time.Millisecond))
	if accepted, err := st.BindVersion(ctx, "sess", "old", old, time.Hour, 3); err != nil || accepted {
		t.Fatalf("over-age bind accepted=%v err=%v", accepted, err)
	}
	// Expire all metadata, recreate generation zero, and roll time back so
	// age alone cannot save us: the random incarnation must prevent ABA.
	mr.FastForward(AffinityMetadataTTL + time.Millisecond)
	if mr.Exists(meta) {
		t.Fatal("metadata did not expire")
	}
	mr.SetTime(base)
	_, fresh, err := st.RecentVersion(ctx, "sess", 3)
	if err != nil || fresh == old {
		t.Fatalf("recreated epoch: %q, %v", fresh, err)
	}
	if accepted, err := st.BindVersion(ctx, "sess", "old", old, time.Hour, 3); err != nil || accepted {
		t.Fatalf("expired epoch ABA accepted=%v err=%v", accepted, err)
	}
	if accepted, err := st.BindVersion(ctx, "sess", "fresh", fresh, time.Hour, 3); err != nil || !accepted {
		t.Fatalf("fresh epoch accepted=%v err=%v", accepted, err)
	}
	for _, key := range mr.Keys() {
		if ttl := mr.TTL(key); ttl <= 0 {
			t.Fatalf("unbounded Redis metadata/key %q ttl=%v", key, ttl)
		}
	}
}

func TestAffinityStoreKeysShareHashSlotAndMetadataIsPerConversation(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := Open(context.Background(), mr.Addr(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	st := NewAffinityStore(c.Client)
	keys := st.keys("secret{injected}")
	tag := func(key string) string {
		start := strings.IndexByte(key, '{')
		end := strings.IndexByte(key, '}')
		if start < 0 || end <= start {
			t.Fatalf("missing hash tag: %q", key)
		}
		return key[start+1 : end]
	}
	if tag(keys[0]) != tag(keys[1]) || strings.Contains(keys[0], "secret") {
		t.Fatalf("unsafe/non-colocated keys: %v", keys)
	}
	for i := 0; i < 100; i++ {
		if err := st.Demote(context.Background(), "secret{injected}", strings.Repeat("x", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if got := mr.Keys(); len(got) != 1 {
		t.Fatalf("per-uid tombstones leaked: %v", got)
	}
	if fields, err := c.Client.HLen(context.Background(), keys[1]).Result(); err != nil || fields != 2 {
		t.Fatalf("metadata fields=%d err=%v", fields, err)
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
