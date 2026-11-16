package ai

import (
	"testing"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

// Production signature (2026-11-16, conversation with the Outlook bridge): the
// device's UI isolate parks on an unbounded syscall, so from then on every
// client tool call goes unpaid. The stall watchdog fires at 90s, synthesizes a
// timeout, relaunches the loop, the model issues the next device call, and the
// server pays the full stall budget AGAIN — per iteration, plus a provider
// round-trip. The client-side fix bounds individual syscalls, but a wedge from
// an older deployed client leaves the server spending 90s per call on a device
// that cannot answer. The memo bounds that: inside the window, with nobody
// attached to the loop, dispatch is skipped and the model gets the answer
// immediately.
func TestClientToolTimeoutMemoBoundsTheStallLoop(t *testing.T) {
	memo := &clientToolTimeoutMemo{seen: make(map[string]time.Time)}

	if memo.recentlyTimedOut("conv-a") {
		t.Fatal("unknown conversation must not be memoized")
	}
	memo.note("conv-a")
	if !memo.recentlyTimedOut("conv-a") {
		t.Fatal("just-timed-out conversation must be memoized")
	}
	if memo.recentlyTimedOut("conv-b") {
		t.Fatal("memo must be per-conversation, not global")
	}

	memo.seen["conv-a"] = time.Now().Add(-clientToolTimeoutMemoWindow - time.Second)
	if memo.recentlyTimedOut("conv-a") {
		t.Fatal("window must expire so a recovered device is dispatched again")
	}
	if _, ok := memo.seen["conv-a"]; ok {
		t.Fatal("expired entry must be dropped on read")
	}

	memo.note("conv-a")
	memo.clear("conv-a")
	if memo.recentlyTimedOut("conv-a") {
		t.Fatal("an answered call must clear the memo — the device is alive again")
	}

	// Empty ids must not poison the map with a shared "" key.
	memo.note("")
	memo.clear("")
	if memo.recentlyTimedOut("") {
		t.Fatal("empty conversation id must never be memoized")
	}
}

// note() is called from the watchdog goroutine and read/cleared from the loop
// and HandleChat goroutines; -race over concurrent access is the point.
func TestClientToolTimeoutMemoIsConcurrencySafe(t *testing.T) {
	memo := &clientToolTimeoutMemo{seen: make(map[string]time.Time)}
	const n = 64
	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			id := "conv-" + string(rune('a'+i%8))
			memo.note(id)
			memo.recentlyTimedOut(id)
			memo.clear(id)
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}
}

// note() must not leak: every conversation that ever stalls would otherwise
// leave an entry forever.
func TestClientToolTimeoutMemoNoteEvictsStaleEntries(t *testing.T) {
	memo := &clientToolTimeoutMemo{seen: make(map[string]time.Time)}
	memo.seen["stale-1"] = time.Now().Add(-2 * clientToolTimeoutMemoWindow)
	memo.seen["stale-2"] = time.Now().Add(-2 * clientToolTimeoutMemoWindow)
	memo.note("fresh")
	if len(memo.seen) != 1 {
		t.Fatalf("note() must evict stale entries, map has %d: %v", len(memo.seen), memo.seen)
	}
	if !memo.recentlyTimedOut("fresh") {
		t.Fatal("the entry just noted must survive eviction")
	}
}

func TestClientToolDeviceUnresponsiveMessageIsActionable(t *testing.T) {
	msg := clientToolDeviceUnresponsive("shell_client__exec")
	for _, want := range []string{
		"shell_client__exec",
		"system_tools__shell_exec",
		"filesystem_server__fs_read",
		"ask the user",
	} {
		if !containsString(msg, want) {
			t.Fatalf("message must steer the model with %q; got: %s", want, msg)
		}
	}
	if !containsString(msg, clientToolStallTimeout.String()) {
		t.Fatalf("message must state the stall cost the model is being saved from: %s", msg)
	}
}

func containsString(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// The memo window must not exceed the stall budget: a device call that IS
// dispatched still pays the stall timeout, so a memo longer than that would
// keep fast-failing a device that has demonstrably recovered.
func TestClientToolTimeoutMemoWindowIsBounded(t *testing.T) {
	if clientToolTimeoutMemoWindow <= 0 {
		t.Fatal("memo window must be positive")
	}
	if clientToolTimeoutMemoWindow > clientToolStallTimeout {
		t.Fatalf("memo window %v must not exceed the stall timeout %v",
			clientToolTimeoutMemoWindow, clientToolStallTimeout)
	}
}

// Guard the shared synthesizer against a nil conversation pointer: the loop
// passes &conversation and the watchdog reloads the conversation, so a nil
// deref would panic the goroutine and kill the turn.
func TestSynthesizeClientToolResultGuardsNil(t *testing.T) {
	// No HTTP writer attached: Broadcast must be a no-op, and a nil
	// conversation must not be written through.
	ar := NewActiveRequest("conv-nil", "u1", utils.Model{}, utils.ModelSelected{})
	defer ar.Cancel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("synthesizeClientToolResult panicked: %v", r)
		}
	}()
	synthesizeClientToolResult(ar.Ctx, ar, nil, utils.ToolCall{ID: "call_0", Function: utils.FunctionCall{Name: "shell_client__exec"}}, "boom")
}
