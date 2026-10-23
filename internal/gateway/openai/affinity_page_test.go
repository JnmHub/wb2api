package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
)

// A conversation past WB_AFFINITY_MAX_MSGS used to ignore session affinity
// entirely: every turn rotated over its page, so turns repeatedly landed on
// page accounts that had never seen the prefix and reported zero cached
// tokens. The page must still bound the accounts that carry the large bodies,
// but within it the conversation has to stay on the account that already holds
// its prefix.
func TestOverLimitConversationStaysOnWarmAccount(t *testing.T) {
	var mu sync.Mutex
	var tokens []string
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokens = append(tokens, r.Header.Get("Authorization"))
		mu.Unlock()
		sseUpstream("x")(w, r)
	})
	e := newEnv(t, up, 12, "")

	send := func(n int) {
		msgs := make([]any, n)
		for i := range msgs {
			msgs[i] = map[string]any{"role": "user", "content": fmt.Sprintf("turn %d", i)}
		}
		// The head is what the affinity key hashes; keep it stable across turns.
		msgs[0] = map[string]any{"role": "system", "content": "stable system"}
		msgs[1] = map[string]any{"role": "user", "content": "stable first question"}
		body, err := json.Marshal(map[string]any{
			"model": "glm-5.3", "messages": msgs, "stream": true,
		})
		if err != nil {
			t.Fatal(err)
		}
		readSSE(t, e.post(t, string(body), nil))
	}
	send(401)
	send(402)

	mu.Lock()
	defer mu.Unlock()
	if len(tokens) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(tokens))
	}
	if tokens[0] != tokens[1] {
		t.Fatalf("over-limit conversation moved accounts: %s then %s", tokens[0], tokens[1])
	}
	if n := e.h.Pool.Affinity().Len(); n == 0 {
		t.Fatal("over-limit conversation did not publish an affinity binding")
	}
}

// Below the cap nothing changes: the conversation pins to the account that
// served its first turn.
func TestUnderLimitConversationPinsToWarmAccount(t *testing.T) {
	var mu sync.Mutex
	var tokens []string
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokens = append(tokens, r.Header.Get("Authorization"))
		mu.Unlock()
		sseUpstream("x")(w, r)
	})
	e := newEnv(t, up, 12, "")

	send := func(n int) {
		msgs := make([]any, n)
		for i := range msgs {
			msgs[i] = map[string]any{"role": "user", "content": fmt.Sprintf("turn %d", i)}
		}
		msgs[0] = map[string]any{"role": "system", "content": "stable system"}
		msgs[1] = map[string]any{"role": "user", "content": "stable first question"}
		body, err := json.Marshal(map[string]any{
			"model": "glm-5.3", "messages": msgs, "stream": true,
		})
		if err != nil {
			t.Fatal(err)
		}
		readSSE(t, e.post(t, string(body), nil))
	}
	send(3)
	send(5)

	mu.Lock()
	defer mu.Unlock()
	if len(tokens) != 2 || tokens[0] != tokens[1] {
		t.Fatalf("under-limit conversation moved accounts: %v", tokens)
	}
}
