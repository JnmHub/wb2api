package events

import (
	"bufio"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func recv(t *testing.T, s *Sub) Event {
	t.Helper()
	select {
	case ev := <-s.C:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
		return Event{}
	}
}

func TestPublishSubscribe(t *testing.T) {
	b := New(8)
	s := b.Subscribe(0)
	defer s.Close()
	b.Publish("a", map[string]int{"x": 1})
	b.Publish("b", "text")
	e1, e2 := recv(t, s), recv(t, s)
	if e1.Type != "a" || string(e1.Data) != `{"x":1}` || e2.Type != "b" || e2.ID != e1.ID+1 {
		t.Fatalf("%+v %+v", e1, e2)
	}
	if b.Subscribers() != 1 {
		t.Fatal("subscriber count")
	}
	s.Close()
	if b.Subscribers() != 0 {
		t.Fatal("close must unsubscribe")
	}
}

func TestSlowSubscriberNeverBlocksAndIsToldToResync(t *testing.T) {
	b := New(8)
	s := b.Subscribe(0)
	defer s.Close()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10_000; i++ {
			b.Publish("e", i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Publish blocked on a subscriber that does not read")
	}
	if !s.Lagged() {
		t.Fatal("a subscriber that lost events must be marked lagged")
	}
	if s.Lagged() {
		t.Fatal("Lagged clears the flag")
	}
}

func TestReplayAfterLastEventID(t *testing.T) {
	b := New(4)
	for i := 0; i < 3; i++ {
		b.Publish("e", i)
	}
	s := b.Subscribe(1) // saw event 1, missed 2 and 3
	defer s.Close()
	if recv(t, s).ID != 2 || recv(t, s).ID != 3 {
		t.Fatal("missed events must be replayed in order")
	}
	if s.Lagged() {
		t.Fatal("a complete replay needs no resync")
	}
	for i := 0; i < 10; i++ {
		b.Publish("e", i)
	}
	old := b.Subscribe(2) // far outside the replay window now
	defer old.Close()
	if !old.Lagged() {
		t.Fatal("an id outside the window must trigger a resync")
	}
	cur := b.Subscribe(b.seq.Load())
	defer cur.Close()
	if cur.Lagged() || len(cur.C) != 0 {
		t.Fatal("up to date: nothing to replay")
	}
}

func TestSSEHandlerStreamsEvents(t *testing.T) {
	b := New(8)
	srv := httptest.NewServer(b.Handler(50 * time.Millisecond))
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	br := bufio.NewReader(resp.Body)
	var got strings.Builder
	// wait for the stream to be established, then publish
	deadline := time.Now().Add(2 * time.Second)
	for b.Subscribers() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	b.Publish("account.patch", map[string]string{"uid": "u1"})
	for !strings.Contains(got.String(), "data: {\"uid\":\"u1\"}") {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended: %v; got %q", err, got.String())
		}
		got.WriteString(line)
	}
	if !strings.Contains(got.String(), "retry: 3000") || !strings.Contains(got.String(), "event: account.patch") ||
		!strings.Contains(got.String(), "id: 1") {
		t.Fatalf("bad frame: %q", got.String())
	}
	// heartbeat arrives on an idle stream
	for !strings.Contains(got.String(), ": keep-alive") {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("no heartbeat: %v", err)
		}
		got.WriteString(line)
	}
}

func TestCoalescer(t *testing.T) {
	var c Coalescer
	if c.Take() != nil {
		t.Fatal("empty")
	}
	for i := 0; i < 100; i++ {
		c.Mark("a")
		c.Mark("b")
	}
	got := c.Take()
	if len(got) != 2 {
		t.Fatalf("100 marks of 2 keys must collapse to 2: %v", got)
	}
	if c.Take() != nil {
		t.Fatal("Take clears")
	}
}
