package ai

import (
	"testing"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

// The device channel exists so a background run can reach the user's app
// (docs/device-channel.md). Everything below is the claim's contract: one live
// claim per user, freshest wins, TTL-bounded with lazy expiry, per-user
// isolation. A stale claim must be invisible: dispatching into a dead app is
// the hang the fast-fail path was built to avoid.

func tool(name string) utils.FunctionToolsRequest {
	return utils.FunctionToolsRequest{Name: name, Description: name + " desc"}
}

func resetDeviceRegistry(t *testing.T) *deviceRegistry {
	t.Helper()
	r := &deviceRegistry{claims: make(map[string]*deviceClaim), pending: make(map[string]*devicePending)}
	return r
}

func TestDeviceClaimExpiryIsLazy(t *testing.T) {
	r := resetDeviceRegistry(t)
	exp := r.Claim("u1", "c1", "conn1", "", []utils.FunctionToolsRequest{tool("shell_client__exec")}, time.Minute)
	if exp.IsZero() {
		t.Fatal("claim must report its expiry")
	}
	if !r.HasLiveClaim("u1") {
		t.Fatal("a fresh claim is live")
	}
	if !r.ClaimedTool("shell_client__exec", "u1") {
		t.Fatal("the advertised tool must be claimable")
	}
	if r.ClaimedTool("filesystem_client__fs_read", "u1") {
		t.Fatal("a tool the claim does not advertise must not be dispatchable")
	}

	// Simulate the TTL elapsing (the app stopped refreshing).
	r.claims["u1"].ExpiresAt = time.Now().Add(-time.Second)
	if r.HasLiveClaim("u1") {
		t.Fatal("an elapsed claim must not be live")
	}
	if r.ClaimedTool("shell_client__exec", "u1") {
		t.Fatal("an elapsed claim must not route work")
	}
	if got := r.ToolsForUser("u1"); got != nil {
		t.Fatalf("an elapsed claim must advertise nothing, got %v", got)
	}
	// Lazy expiry must evict, not leak.
	if r.ClaimCount() != 0 {
		t.Fatalf("expired claim must be evicted on read, %d held", r.ClaimCount())
	}
}

// Two devices of the same user: the one refreshing last owns the work. A
// device that stopped refreshing has an elapsed claim and must not steal the
// slot back (it is already dead — handing it work is the hang).
func TestDeviceClaimFreshestWins(t *testing.T) {
	r := resetDeviceRegistry(t)
	r.Claim("u1", "laptop", "connA", `C:\Code`, []utils.FunctionToolsRequest{tool("shell_client__exec")}, time.Minute)
	r.Claim("u1", "desktop", "connB", `D:\Work`, []utils.FunctionToolsRequest{tool("shell_client__exec"), tool("filesystem_client__fs_read")}, time.Minute)

	claim := r.ClaimForUser("u1")
	if claim == nil {
		t.Fatal("expected a live claim")
	}
	if claim.ClientID != "desktop" {
		t.Fatalf("freshest claim must win, got %q", claim.ClientID)
	}
	if claim.ClientFolderPath != `D:\Work` {
		t.Fatalf("folder must follow the winning claim, got %q", claim.ClientFolderPath)
	}
	if r.ClaimCount() != 1 {
		t.Fatalf("exactly one claim per user, got %d", r.ClaimCount())
	}

	// The laptop re-claims AFTER the desktop: it wins (it is now the freshest).
	r.Claim("u1", "laptop", "connA", `C:\Code`, []utils.FunctionToolsRequest{tool("shell_client__exec")}, time.Minute)
	if c := r.ClaimForUser("u1"); c.ClientID != "laptop" {
		t.Fatalf("latest refresh must win, got %q", c.ClientID)
	}

	// A dead device (elapsed claim) refreshing "now" via a stale timestamp must
	// not be able to blind the live one: its re-claim makes it live again,
	// which is correct — it is connected and advertising.
	r.claims["u1"].ExpiresAt = time.Now().Add(-time.Second)
	r.Claim("u1", "desktop", "connB", `D:\Work`, []utils.FunctionToolsRequest{tool("shell_client__exec")}, time.Minute)
	if c := r.ClaimForUser("u1"); c.ClientID != "desktop" {
		t.Fatalf("a live re-claim must win, got %q", c.ClientID)
	}
}

// The claim is per-user: one user's app must never receive another user's
// device calls, and one user's claim must not make another user's tools
// dispatchable.
func TestDeviceClaimIsPerUser(t *testing.T) {
	r := resetDeviceRegistry(t)
	r.Claim("u1", "c1", "conn1", "", []utils.FunctionToolsRequest{tool("shell_client__exec")}, time.Minute)

	if r.HasLiveClaim("u2") {
		t.Fatal("a claim must not leak across users")
	}
	if r.ClaimedTool("shell_client__exec", "u2") {
		t.Fatal("u2 must not be able to dispatch u1's tool")
	}
	if got := r.ToolsForUser("u2"); got != nil {
		t.Fatalf("u2 must advertise nothing, got %v", got)
	}

	// Second user claims its own set; both coexist.
	r.Claim("u2", "c2", "conn2", "/tmp/x", []utils.FunctionToolsRequest{tool("filesystem_client__fs_read")}, time.Minute)
	if !r.ClaimedTool("shell_client__exec", "u1") || !r.ClaimedTool("filesystem_client__fs_read", "u2") {
		t.Fatal("both users must keep their own claims")
	}
	if r.ClaimedTool("filesystem_client__fs_read", "u1") {
		t.Fatal("u1 must not gain u2's tool")
	}
	if r.ClaimCount() != 2 {
		t.Fatalf("2 claims expected, got %d", r.ClaimCount())
	}

	// Empty user / client ids can never create a claim.
	if !r.Claim("", "c", "conn", "", nil, time.Minute).Equal(time.Time{}) {
		t.Fatal("empty userID must not claim")
	}
	if !r.Claim("u3", "", "conn", "", nil, time.Minute).Equal(time.Time{}) {
		t.Fatal("empty clientID must not claim")
	}
	if r.HasLiveClaim("") {
		t.Fatal("empty userID must never be live")
	}
}

// The claim dies with its status-stream connection (spec: "the claim lives
// exactly as long as its status-stream connection"), so a background run of a
// disconnected app fails fast on the existing fast-fail path instead of
// dispatching into a closed socket.
func TestDeviceClaimDiesWithConnection(t *testing.T) {
	r := resetDeviceRegistry(t)
	r.ClaimOnConnect("u1", "c1", "conn1")
	if !r.HasLiveClaim("u1") {
		t.Fatal("a connecting client owns a claim slot")
	}
	if got := r.ToolsForUser("u1"); got != nil {
		t.Fatalf("the connect placeholder advertises no tools (the app posts them via /client/claim), got %v", got)
	}

	// The app then posts its real tool list on that connection.
	r.Claim("u1", "c1", "conn1", `C:\Code`, []utils.FunctionToolsRequest{tool("shell_client__exec")}, time.Minute)
	if !r.ClaimedTool("shell_client__exec", "u1") {
		t.Fatal("the posted claim must advertise the tool")
	}

	// A reconnect of the SAME client must not downgrade the advertisement.
	r.ClaimOnConnect("u1", "c1", "conn2")
	if !r.ClaimedTool("shell_client__exec", "u1") {
		t.Fatal("ClaimOnConnect must not wipe an existing tool list")
	}
	// The claim now belongs to the new socket; closing the OLD one must not
	// blind the user (reconnect race: old socket's Remove arrives late).
	r.DropForConn("conn1")
	if !r.ClaimedTool("shell_client__exec", "u1") {
		t.Fatal("dropping a superseded connection must not drop the claim")
	}
	// Closing the socket that owns the claim does drop it.
	r.DropForConn("conn2")
	if r.HasLiveClaim("u1") {
		t.Fatal("the claim must die with its connection")
	}
	// DropForConn("") is a no-op (legacy clients have no conn binding).
	r.Claim("u1", "c1", "", "", []utils.FunctionToolsRequest{tool("shell_client__exec")}, time.Minute)
	r.DropForConn("")
	if !r.HasLiveClaim("u1") {
		t.Fatal("empty connID must not match any claim")
	}
}

func TestClampDeviceClaimTTL(t *testing.T) {
	cases := []struct {
		seconds int
		want    time.Duration
	}{
		{0, deviceClaimTTLDefault},  // omitted -> default
		{-5, deviceClaimTTLDefault}, // nonsense -> default
		{120, 120 * time.Second},    // spec default
		{14, deviceClaimTTLMin},     // below floor -> clamped
		{15, deviceClaimTTLMin},     // floor
		{600, deviceClaimTTLMax},    // ceiling
		{601, deviceClaimTTLMax},    // above ceiling -> clamped
		{30, 30 * time.Second},      // the app's ~30s refresh cadence is legal
	}
	for _, tc := range cases {
		if got := clampDeviceClaimTTL(tc.seconds); got != tc.want {
			t.Fatalf("clampDeviceClaimTTL(%d) = %v, want %v", tc.seconds, got, tc.want)
		}
	}
	// The spec's cadence must be shorter than the default TTL, otherwise a
	// refresh would arrive after the claim already lapsed.
	if deviceClaimTTLDefault <= 30*time.Second {
		t.Fatalf("default TTL %v must exceed the ~30s refresh cadence", deviceClaimTTLDefault)
	}
	// The dispatch timeout must be bounded by the chat path's watchdog spirit:
	// long enough for unattended work, short enough to not wedge a run forever.
	if deviceToolTimeout <= clientToolStallTimeout {
		t.Fatalf("device dispatch timeout %v should exceed the chat stall timeout %v (unattended calls)", deviceToolTimeout, clientToolStallTimeout)
	}
	if deviceToolTimeout > 10*time.Minute {
		t.Fatalf("device dispatch timeout %v is too long for an unattended loop", deviceToolTimeout)
	}
}

// Prompt-cache contract: a claim's tool list becomes payload.ClientSideTools
// and is rendered into the prompt prefix by ai_tools.GetRequests. Go
// randomizes map iteration order, so an unsorted claim list would byte-shuffle
// the tools array every turn and collapse KV prefix reuse (see
// ai_tools/registry.go). ToolsForUser must therefore be sorted and stable.
func TestToolsForUserIsSortedAndStable(t *testing.T) {
	r := resetDeviceRegistry(t)
	// Deliberately unsorted + duplicated on the way in.
	in := []utils.FunctionToolsRequest{
		tool("shell_client__exec"),
		tool("filesystem_client__fs_write"),
		tool("github__get_me"),
		tool("filesystem_client__fs_read"),
		tool("shell_client__exec"), // duplicate name, first wins
		{Name: ""},                 // nameless entry is dropped
	}
	r.Claim("u1", "c1", "conn1", "", in, time.Minute)

	var first []string
	const trials = 40
	for i := 0; i < trials; i++ {
		got := r.ToolsForUser("u1")
		names := make([]string, 0, len(got))
		for _, f := range got {
			names = append(names, f.Name)
		}
		if i == 0 {
			first = names
			continue
		}
		if len(names) != len(first) {
			t.Fatalf("trial %d: size changed %d -> %d", i, len(first), len(names))
		}
		for j := range names {
			if names[j] != first[j] {
				t.Fatalf("trial %d: order changed at %d: %v vs %v", i, j, first, names)
			}
		}
	}

	want := []string{"filesystem_client__fs_read", "filesystem_client__fs_write", "github__get_me", "shell_client__exec"}
	if len(first) != len(want) {
		t.Fatalf("got %d tools, want %d: %v", len(first), len(want), first)
	}
	for i := range want {
		if first[i] != want[i] {
			t.Fatalf("not sorted by name at %d: got %v, want %v", i, first, want)
		}
	}
}

func TestIntersectClientSideTools(t *testing.T) {
	claim := []utils.FunctionToolsRequest{
		tool("shell_client__exec"),
		tool("filesystem_client__fs_read"),
		tool("filesystem_client__fs_write"),
	}
	cases := []struct {
		name    string
		allowed []utils.FunctionToolsRequest
		want    []string
	}{
		{
			name:    "parent had only the shell",
			allowed: []utils.FunctionToolsRequest{tool("shell_client__exec")},
			want:    []string{"shell_client__exec"},
		},
		{
			name:    "parent set is unsorted input -> sorted output",
			allowed: []utils.FunctionToolsRequest{tool("filesystem_client__fs_write"), tool("filesystem_client__fs_read")},
			want:    []string{"filesystem_client__fs_read", "filesystem_client__fs_write"},
		},
		{
			// Empty allowed set = "the parent's advertised set is unknown"
			// (the loop's payload is not readable from outside it), so the
			// claim passes through: model.Tools gating in GetRequests is what
			// keeps the sub-agent from actually seeing a tool the parent lacked.
			name:    "no parent set known: claim passes through normalized",
			allowed: nil,
			want:    []string{"filesystem_client__fs_read", "filesystem_client__fs_write", "shell_client__exec"},
		},
		{
			name:    "parent tool the app does not claim is a no-op",
			allowed: []utils.FunctionToolsRequest{tool("search_web")},
			want:    nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IntersectClientSideTools(claim, tc.allowed)
			var names []string
			for _, f := range got {
				names = append(names, f.Name)
			}
			if len(names) != len(tc.want) {
				t.Fatalf("got %v, want %v", names, tc.want)
			}
			for i := range tc.want {
				if names[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", names, tc.want)
				}
			}
		})
	}

	// Empty input stays empty (nil), never an empty non-nil slice that would
	// read as "advertised, with zero tools" to callers.
	if got := IntersectClientSideTools(nil, []utils.FunctionToolsRequest{tool("x")}); got != nil {
		t.Fatalf("empty tools must yield nil, got %v", got)
	}
}

// --- pending requests ---

// A dispatched call waits on its channel; Resolve delivers exactly once and
// removes the entry, so a replayed POST cannot answer the same call twice.
func TestPendingRegisterResolve(t *testing.T) {
	r := resetDeviceRegistry(t)
	results, cleanup := r.RegisterPending("req1", "u1", "conv1", "call_0", "shell_client__exec", time.Minute)
	defer cleanup()

	if r.PendingCount() != 1 {
		t.Fatalf("1 pending expected, got %d", r.PendingCount())
	}
	if !r.Resolve("req1", "u1", "call_0", "shell_client__exec", "hello", "") {
		t.Fatal("matching resolve must succeed")
	}
	select {
	case got := <-results:
		if got.Result != "hello" || got.Err != "" {
			t.Fatalf("got %+v", got)
		}
	default:
		t.Fatal("result must be delivered")
	}
	if r.PendingCount() != 0 {
		t.Fatalf("resolved request must be removed, %d held", r.PendingCount())
	}
	if r.Resolve("req1", "u1", "call_0", "shell_client__exec", "again", "") {
		t.Fatal("a resolved request must not answer twice")
	}
}

// Authorization + integrity: a result from the wrong user, for the wrong call
// id, or for the wrong tool name is rejected (the endpoint answers 404).
func TestPendingResolveRejectsMismatchedClaims(t *testing.T) {
	r := resetDeviceRegistry(t)
	_, cleanup := r.RegisterPending("req1", "u1", "conv1", "call_0", "shell_client__exec", time.Minute)
	defer cleanup()

	cases := []struct {
		name       string
		userID     string
		toolCallID string
		toolName   string
	}{
		{"another user", "u2", "call_0", "shell_client__exec"},
		{"empty user", "", "call_0", "shell_client__exec"},
		{"foreign tool_call_id", "u1", "call_9", "shell_client__exec"},
		{"foreign tool name", "u1", "call_0", "filesystem_client__fs_read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if r.Resolve("req1", tc.userID, tc.toolCallID, tc.toolName, "x", "") {
				t.Fatal("mismatched resolve must be rejected")
			}
		})
	}
	// The legitimate answer still works after all those rejections.
	if !r.Resolve("req1", "u1", "call_0", "shell_client__exec", "ok", "") {
		t.Fatal("the owner must still be able to answer")
	}
}

// An expired pending request is one the loop already synthesized a timeout for.
// Delivering it would append a second tool row to a turn that moved on, so it
// must be refused and evicted.
func TestPendingExpiry(t *testing.T) {
	r := resetDeviceRegistry(t)
	_, cleanup := r.RegisterPending("req1", "u1", "conv1", "call_0", "shell_client__exec", time.Minute)
	defer cleanup()
	r.pending["req1"].ExpiresAt = time.Now().Add(-time.Second)

	if r.Resolve("req1", "u1", "call_0", "shell_client__exec", "late", "") {
		t.Fatal("an expired request must not be answered")
	}
	if r.PendingCount() != 0 {
		t.Fatalf("expired request must be evicted, %d held", r.PendingCount())
	}
}

// Resolve must never block on a consumer that walked away (timeout / cancel):
// the buffered channel is what makes POST /chat/device-tool-result return in
// bounded time even after the loop stopped listening.
func TestPendingResolveDoesNotBlockAfterConsumerGone(t *testing.T) {
	r := resetDeviceRegistry(t)
	results, cleanup := r.RegisterPending("req1", "u1", "conv1", "call_0", "shell_client__exec", time.Minute)
	_ = results // nobody reads it; the loop timed out
	done := make(chan struct{})
	go func() {
		r.Resolve("req1", "u1", "call_0", "shell_client__exec", "late answer", "")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Resolve blocked on an abandoned request")
	}
	cleanup()
}

func TestCombineDeviceToolResult(t *testing.T) {
	cases := []struct {
		result, err, want string
	}{
		{"out", "", "out"},
		{"", "boom", "boom"},
		{"out", "boom", "out\nboom"},
		{"", "", ""},
	}
	for _, tc := range cases {
		if got := combineDeviceToolResult(tc.result, tc.err); got != tc.want {
			t.Fatalf("combineDeviceToolResult(%q,%q) = %q, want %q", tc.result, tc.err, got, tc.want)
		}
	}
}
