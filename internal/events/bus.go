// Package events is the dashboard's live channel: server-sent events replace
// the legacy panel's full-table polling (every 5-15 s, whatever had changed).
//
// Publishing never blocks a request. A subscriber that cannot keep up misses
// events and is told to resync, which makes the client refetch instead of
// silently drifting out of date.
package events

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Event is one message.
type Event struct {
	ID   uint64
	Type string
	Data json.RawMessage
}

// Bus fans events out to subscribers and keeps a short replay window.
type Bus struct {
	seq atomic.Uint64

	mu   sync.Mutex
	subs map[*Sub]struct{}
	ring []Event // most recent events, oldest first
	max  int
}

// New creates a Bus remembering the last replay events.
func New(replay int) *Bus {
	if replay <= 0 {
		replay = 512
	}
	return &Bus{subs: map[*Sub]struct{}{}, max: replay}
}

// Sub is one subscriber.
type Sub struct {
	C      chan Event
	bus    *Bus
	lagged atomic.Bool
}

// Publish sends an event to every subscriber without blocking.
func (b *Bus) Publish(typ string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	ev := Event{ID: b.seq.Add(1), Type: typ, Data: raw}
	b.mu.Lock()
	b.ring = append(b.ring, ev)
	if len(b.ring) > b.max {
		b.ring = append(b.ring[:0], b.ring[len(b.ring)-b.max:]...)
	}
	for s := range b.subs {
		select {
		case s.C <- ev:
		default:
			s.lagged.Store(true) // dropped: the consumer must resync
		}
	}
	b.mu.Unlock()
}

// Subscribe registers a subscriber. If afterID is within the replay window the
// missed events are delivered first; otherwise the first event is "resync".
func (b *Bus) Subscribe(afterID uint64) *Sub {
	s := &Sub{C: make(chan Event, 256), bus: b}
	b.mu.Lock()
	defer b.mu.Unlock()
	if afterID > 0 {
		oldest := uint64(0)
		if len(b.ring) > 0 {
			oldest = b.ring[0].ID
		}
		switch {
		case afterID >= b.seq.Load():
			// nothing missed
		case len(b.ring) > 0 && afterID+1 >= oldest:
			for _, ev := range b.ring {
				if ev.ID > afterID {
					s.C <- ev
				}
			}
		default:
			s.lagged.Store(true)
		}
	}
	b.subs[s] = struct{}{}
	return s
}

// Close unsubscribes.
func (s *Sub) Close() {
	s.bus.mu.Lock()
	delete(s.bus.subs, s)
	s.bus.mu.Unlock()
}

// Lagged reports (and clears) whether events were dropped for this subscriber.
func (s *Sub) Lagged() bool { return s.lagged.Swap(false) }

// Subscribers returns the current number of subscribers.
func (b *Bus) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// Handler serves GET as a text/event-stream. Heartbeat comments keep proxies
// from closing an idle stream.
func (b *Bus) Handler(heartbeat time.Duration) http.Handler {
	if heartbeat <= 0 {
		heartbeat = 20 * time.Second
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Accel-Buffering", "no")
		var after uint64
		if v := r.Header.Get("Last-Event-ID"); v != "" {
			after, _ = strconv.ParseUint(v, 10, 64)
		}
		sub := b.Subscribe(after)
		defer sub.Close()
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "retry: 3000\n\n")
		if sub.Lagged() {
			fmt.Fprintf(w, "event: resync\ndata: {}\n\n")
		}
		_ = rc.Flush()
		tick := time.NewTicker(heartbeat)
		defer tick.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case ev := <-sub.C:
				if sub.Lagged() {
					fmt.Fprintf(w, "event: resync\ndata: {}\n\n")
				}
				fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, ev.Type, ev.Data)
				// Drain whatever is already queued before flushing: one write
				// per burst instead of one per event.
			drain:
				for {
					select {
					case ev := <-sub.C:
						fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, ev.Type, ev.Data)
					default:
						break drain
					}
				}
				if err := rc.Flush(); err != nil {
					return
				}
			case <-tick.C:
				if sub.Lagged() {
					fmt.Fprintf(w, "event: resync\ndata: {}\n\n")
				} else {
					fmt.Fprint(w, ": keep-alive\n\n")
				}
				if err := rc.Flush(); err != nil {
					return
				}
			}
		}
	})
}

// Coalescer collects "this key changed" marks and emits them once per interval
// as a single batch, so a burst of changes to one account costs one message.
type Coalescer struct {
	mu    sync.Mutex
	dirty map[string]struct{}
}

// Mark records that key changed. It is cheap and never blocks.
func (c *Coalescer) Mark(key string) {
	c.mu.Lock()
	if c.dirty == nil {
		c.dirty = map[string]struct{}{}
	}
	c.dirty[key] = struct{}{}
	c.mu.Unlock()
}

// Take returns and clears the marked keys.
func (c *Coalescer) Take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.dirty) == 0 {
		return nil
	}
	out := make([]string, 0, len(c.dirty))
	for k := range c.dirty {
		out = append(out, k)
	}
	c.dirty = nil
	return out
}
