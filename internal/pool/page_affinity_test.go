package pool

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// pageGroup returns the uids a page covers for the realm.
func pageGroup(p *Pool, realm string, page *Page) map[string]bool {
	out := map[string]bool{}
	prefs := p.preferences().byRealm[realm]
	if prefs == nil {
		return out
	}
	for uid, r := range prefs.rank {
		if r%page.Size == page.Index%page.Size {
			out[uid] = true
		}
	}
	return out
}

func TestColdPageConversationIsServedInsideItsPage(t *testing.T) {
	p := New(mkRecs(12, time.Hour))
	page := &Page{Index: 0, Size: 3}
	group := pageGroup(p, "intl", page)
	if len(group) != 4 {
		t.Fatalf("page holds %d accounts, want 4", len(group))
	}
	for i := 0; i < 24; i++ {
		key := fmt.Sprintf("pfx-cold-%d", i)
		a := p.SelectForSessionBestContext(context.Background(), "intl", key, nil, "m", page, nil)
		if a == nil {
			t.Fatalf("%s: nil selection", key)
		}
		if !group[a.UID] {
			t.Fatalf("%s: served %s outside its page %v", key, a.UID, group)
		}
	}
}

func TestPageConversationPrefersWarmAccount(t *testing.T) {
	p := New(mkRecs(12, time.Hour))
	page := &Page{Index: 0, Size: 3}
	group := pageGroup(p, "intl", page)

	// First turn (cold) lands somewhere inside the page and is bound afterwards,
	// the way the gateway binds once the upstream accepted the request.
	first := p.SelectForSessionBestContext(context.Background(), "intl", "pfx-warm", nil, "m", page, nil)
	if first == nil || !group[first.UID] {
		t.Fatalf("cold turn served outside the page: %+v", first)
	}
	p.Affinity().Bind("pfx-warm", first.UID)

	// Later turns must keep using that account even though the page rotation
	// cursor has moved on.
	for i := 0; i < 10; i++ {
		got := p.SelectForSessionBestContext(context.Background(), "intl", "pfx-warm", nil, "m", page, nil)
		if got == nil || got.UID != first.UID {
			t.Fatalf("turn %d served by %v, want warm %s", i, got, first.UID)
		}
	}
}

func TestWarmAccountOutsidePageDoesNotEscapeIt(t *testing.T) {
	p := New(mkRecs(12, time.Hour))
	page := &Page{Index: 1, Size: 3}
	group := pageGroup(p, "intl", page)

	var outside string
	for uid, r := range p.preferences().byRealm["intl"].rank {
		if r%page.Size != page.Index%page.Size {
			outside = uid
			break
		}
	}
	if outside == "" {
		t.Fatal("fixture has no account outside the page")
	}
	p.Affinity().Bind("pfx-out", outside)

	got := p.SelectForSessionBestContext(context.Background(), "intl", "pfx-out", nil, "m", page, nil)
	if got == nil {
		t.Fatal("nil selection")
	}
	if got.UID == outside {
		t.Fatalf("served %s outside its page; a stale warm binding must not widen the page", outside)
	}
	if !group[got.UID] {
		t.Fatalf("served %s outside page group %v", got.UID, group)
	}
}

func TestPickForSessionBindsPageConversation(t *testing.T) {
	p := New(mkRecs(12, time.Hour))
	page := &Page{Index: 2, Size: 3}
	a := p.PickForSessionBestContext(context.Background(), "intl", "pfx-bind", nil, "m", page, nil)
	if a == nil {
		t.Fatal("nil selection")
	}
	if got := p.Affinity().Get("pfx-bind"); got != a.UID {
		t.Fatalf("binding = %q, want %q", got, a.UID)
	}
}
