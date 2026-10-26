package ai

import (
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// The "agent answered me multiple times at once" bug: two RunLLMLoop
// goroutines streaming the same conversation to the same client. Both fixes
// live in the registry: a claim must report who it displaced, and a loop's
// cleanup must never unregister its successor.

func newTestAR(id string) *ActiveRequest {
	return NewActiveRequest(id, "user-1", utils.Model{Name: "test"}, utils.ModelSelected{})
}

func TestRegistryRegisterReportsDisplaced(t *testing.T) {
	r := &activeRequestRegistry{requests: make(map[string]*ActiveRequest)}
	first := newTestAR("c1")

	prev, replaced := r.Register("c1", first)
	if prev != nil || replaced {
		t.Fatalf("first claim reported a displacement: prev=%v replaced=%v", prev, replaced)
	}

	second := newTestAR("c1")
	prev, replaced = r.Register("c1", second)
	if !replaced || prev != first {
		t.Fatalf("second claim must report the first: prev=%p replaced=%v", prev, replaced)
	}
	if r.Get("c1") != second {
		t.Fatal("registry must hold the latest claimant")
	}
}

func TestRegistryRemoveIfCurrentKeepsSuccessor(t *testing.T) {
	r := &activeRequestRegistry{requests: make(map[string]*ActiveRequest)}
	first := newTestAR("c1")
	r.Register("c1", first)

	second := newTestAR("c1")
	r.Register("c1", second)

	// The superseded loop's deferred cleanup runs second: it must NOT drop
	// the loop that replaced it, or cancel/reconnect go blind and the next
	// POST starts a third loop on the same conversation.
	if r.RemoveIfCurrent("c1", first) {
		t.Fatal("superseded loop must not unregister its successor")
	}
	if r.Get("c1") != second {
		t.Fatal("successor lost from registry")
	}
	if !r.RemoveIfCurrent("c1", second) {
		t.Fatal("current owner must be removable")
	}
	if r.Get("c1") != nil {
		t.Fatal("registry entry must be gone after the owner removes it")
	}
}

func TestAddClientIfOpenRefusesClosedRequest(t *testing.T) {
	ar := newTestAR("c1")
	client := &SSEClient{Done: make(chan struct{}), outQ: make(chan SSEEvent, sseOutQueueDepth), heartbeatStop: make(chan struct{})}

	if !ar.AddClientIfOpen(client) {
		t.Fatal("live request must accept a client")
	}
	if ar.ClientCount() != 1 {
		t.Fatalf("expected 1 client, got %d", ar.ClientCount())
	}

	// Loop exits: cleanup closes clients. An adoption attempt that races this
	// must be refused so the caller starts its own loop instead of parking a
	// client on a stream that will never emit again.
	ar.CloseAllClients()
	late := &SSEClient{Done: make(chan struct{}), outQ: make(chan SSEEvent, sseOutQueueDepth), heartbeatStop: make(chan struct{})}
	if ar.AddClientIfOpen(late) {
		t.Fatal("closed request must refuse a late attach")
	}
	if ar.ClientCount() != 0 {
		t.Fatalf("closed request must hold no clients, got %d", ar.ClientCount())
	}
}

func TestTailIsUserMessage(t *testing.T) {
	msgs := func(roles ...string) []utils.Message {
		out := make([]utils.Message, 0, len(roles))
		for _, r := range roles {
			out = append(out, utils.Message{Role: r})
		}
		return out
	}

	cases := []struct {
		name string
		in   []utils.Message
		want bool
	}{
		{"empty", nil, false},
		{"ends with assistant (turn complete)", msgs("user", "assistant"), false},
		{"ends with queued user prompt", msgs("user", "assistant", "user"), true},
		{"ends with tool result", msgs("assistant", "tool"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tailIsUserMessage(tc.in); got != tc.want {
				t.Fatalf("tailIsUserMessage = %v, want %v", got, tc.want)
			}
		})
	}
}
