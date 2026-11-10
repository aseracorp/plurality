package ai

import (
	"testing"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

// The dispatch path must only short-circuit a client tool when nobody can run
// it. The case that regressed in production: the stall watchdog resumes a
// parked loop on a FRESH ActiveRequest, which starts with zero SSE clients
// attached. The desktop app is alive (its per-user status stream is connected)
// and is re-attaching its conversation stream right now — so failing the tool
// instantly synthesized an error the model obeyed ("no client is connected"),
// and the loop burned a provider round-trip per iteration on a conversation
// whose device was present.
func TestClientToolUnreachable(t *testing.T) {
	cases := []struct {
		name                string
		advertised          bool
		hasConversationConn bool
		hasStatusConn       bool
		want                bool
	}{
		{"normal dispatch", true, true, true, false},
		{"stream attached, no status stream", true, true, false, false},
		{"live app re-attaching: dispatch, do not fail fast", true, false, true, false},
		{"headless run: cron / sub-agent / abandoned resume", true, false, false, true},
		{"hallucinated name with a fully connected client", false, true, true, true},
		{"hallucinated name, no clients at all", false, false, false, true},
		{"hallucinated name, live app re-attaching", false, false, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clientToolUnreachable(tc.advertised, tc.hasConversationConn, tc.hasStatusConn)
			if got != tc.want {
				t.Fatalf("clientToolUnreachable(advertised=%v, conv=%v, status=%v) = %v, want %v",
					tc.advertised, tc.hasConversationConn, tc.hasStatusConn, got, tc.want)
			}
		})
	}
}

// The reattach grace must stay well under the stall watchdog timeout: the
// watchdog is the last line of defense for a genuinely absent device, and the
// grace window is paid on the dispatch path. If the grace approached the stall
// timeout, every unreachable-device turn would burn the full stall budget
// before the synthetic result appeared.
func TestClientToolReattachGraceIsBounded(t *testing.T) {
	if clientToolReattachGrace >= clientToolStallTimeout {
		t.Fatalf("grace %v must be shorter than the stall timeout %v",
			clientToolReattachGrace, clientToolStallTimeout)
	}
	if clientToolReattachGrace <= 0 {
		t.Fatal("grace must be positive")
	}
}

// waitForClient is the mechanism behind the grace window: it returns
// immediately when a client is already attached, and returns false once the
// deadline passes with nobody attached (the headless case).
func TestWaitForClient(t *testing.T) {
	newAR := func() *ActiveRequest {
		return NewActiveRequest("conv-wait", "u1", utils.Model{}, utils.ModelSelected{})
	}

	t.Run("true with a client attached, without blocking", func(t *testing.T) {
		ar := newAR()
		defer ar.Cancel()
		// A channel-less SSEClient is enough to be counted; startWriter tolerates
		// the nil queue (that is what the Send/stopWriter nil guards exist for).
		ar.AddClient(&SSEClient{Done: make(chan struct{})})
		start := time.Now()
		if !ar.waitForClient(time.Second) {
			t.Fatal("waitForClient must be true when a client is attached")
		}
		if d := time.Since(start); d > 100*time.Millisecond {
			t.Fatalf("waitForClient blocked %v despite an attached client", d)
		}
	})

	t.Run("false once the deadline passes", func(t *testing.T) {
		ar := newAR()
		defer ar.Cancel()
		start := time.Now()
		if ar.waitForClient(200 * time.Millisecond) {
			t.Fatal("waitForClient must be false when nobody attaches")
		}
		if d := time.Since(start); d < 150*time.Millisecond {
			t.Fatalf("waitForClient returned early: %v", d)
		}
	})

	t.Run("cancellation ends the wait promptly", func(t *testing.T) {
		ar := newAR()
		go func() {
			time.Sleep(50 * time.Millisecond)
			ar.Cancel()
		}()
		start := time.Now()
		if ar.waitForClient(10 * time.Second) {
			t.Fatal("no client ever attached; expected false")
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("cancellation did not end the wait promptly: %v", d)
		}
	})
}
