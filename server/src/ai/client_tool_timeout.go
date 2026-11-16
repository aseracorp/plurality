package ai

import (
	"sync"
	"time"
)

// clientToolTimeoutMemoWindow is how long a conversation stays in
// "device just failed to answer" mode after the stall watchdog synthesized a
// client-tool timeout. One full stall budget: during it, dispatching another
// device call is known to cost another 90s of dead waiting; after it the
// device is given another chance (the wedge may have cleared — the blocked
// isolate finished, the laptop woke, the user re-attached).
const clientToolTimeoutMemoWindow = clientToolStallTimeout

// clientToolTimeoutMemo remembers per conversation the last time the stall
// watchdog had to synthesize "Client did not respond within 90s".
//
// The failure mode it bounds is the one production showed on 2026-11-16: the
// device's UI isolate parks on a syscall (sleeping Tailscale/SMB/OneDrive
// share, blocked binary), so EVERY client tool call from then on goes unpaid —
// the watchdog fires, synthesizes a timeout, relaunches the loop, the model
// issues the next device call, and the loop pays the full 90s stall again,
// per iteration, with a provider round-trip on top. The client-side fix
// (#178/#175) bounds individual syscalls, but a wedge that predates it — or
// one from an older deployed client — still leaves the server spending 90s
// per tool call on a device that cannot answer.
//
// Failing fast in this window is safe: the message tells the model exactly
// why, tells it to use a server-side tool, and the window expires so a
// recovered device is usable again without any user action.
type clientToolTimeoutMemo struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

var clientToolTimeouts = &clientToolTimeoutMemo{seen: make(map[string]time.Time)}

// note records that the watchdog synthesized a timeout for this conversation.
// Entries older than the window are dropped here, so the map cannot grow with
// the number of conversations that ever had a stall.
func (m *clientToolTimeoutMemo) note(conversationID string) {
	if conversationID == "" {
		return
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, at := range m.seen {
		if id != conversationID && now.Sub(at) > clientToolTimeoutMemoWindow {
			delete(m.seen, id)
		}
	}
	m.seen[conversationID] = now
}

// recentlyTimedOut reports whether this conversation's device failed to
// answer a client tool call within the current window.
func (m *clientToolTimeoutMemo) recentlyTimedOut(conversationID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	at, ok := m.seen[conversationID]
	if !ok {
		return false
	}
	if time.Since(at) > clientToolTimeoutMemoWindow {
		delete(m.seen, conversationID)
		return false
	}
	return true
}

// clear drops the memo for one conversation. Called when a client-side tool
// result actually arrives from the device: an answered call proves the
// isolate is not parked any more, and the next call should be dispatched
// normally instead of fast-failed for the remainder of the window.
func (m *clientToolTimeoutMemo) clear(conversationID string) {
	if conversationID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.seen, conversationID)
}

// reset clears the memo. Exposed for tests only.
func (m *clientToolTimeoutMemo) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen = make(map[string]time.Time)
}

// clientToolDeviceUnresponsive is the fast-fail message served inside the
// memo window. It is deliberately explicit about the diagnosis and the
// remedy, so the model stops re-issuing device calls instead of burning the
// stall budget on each one.
func clientToolDeviceUnresponsive(toolName string) string {
	return "Error: tool '" + toolName + "' was not dispatched. The device client did not answer its previous " +
		"client-side call, so it is frozen, asleep, or offline (a sleeping Tailscale/SMB/OneDrive share " +
		"or a blocked binary does exactly this), and dispatching now would stall for " +
		clientToolStallTimeout.String() + " again. Use a server-side tool instead (e.g. " +
		"system_tools__shell_exec, filesystem_server__fs_read) or ask the user to wake the app / attach a " +
		"local folder. Device calls are retried automatically once the device answers again."
}
