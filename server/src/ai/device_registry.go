package ai

import (
	"sort"
	"sync"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

// deviceRegistry is the app-wide device channel (DeviceRegistry is the process-wide instance):: per-user live claims ("this
// app can execute these device tools right now") plus the pending device tool
// calls dispatched to a claim and awaiting their result.
//
// Why this exists (docs/device-channel.md): device tools are advertised only
// by an app that streams THAT conversation (ChatPayload.ClientSideTools), so
// every background run — cron, webhook (jobs.RunPrompt), sub-agent
// (jobs.RunSubAgent) — fast-failed with "cannot be executed by this client"
// even while the user's app was running and logged in. Production signature
// (2026-11-19, the Outlook bridge cron chain): the agent concluded "the
// Windows app is offline" from what was really a server-side routing gap. The
// claim makes the app's live capability visible to any run of that user; the
// pending map routes the call to it and blocks in place until the device
// answers.
//
// Lock order: StatusRegistry.mu -> DeviceRegistry.mu (statusRegistry.Add and
// Remove register and drop claims while holding their own lock). No path may
// take DeviceRegistry.mu and then acquire StatusRegistry.mu — dispatchDeviceTool
// reads a claim, releases, and only then broadcasts.
type deviceRegistry struct {
	mu      sync.RWMutex
	claims  map[string]*deviceClaim   // userID -> the winning claim
	pending map[string]*devicePending // requestID -> in-flight device call
}

// deviceClaim is one live advertisement from an app connection.
type deviceClaim struct {
	UserID           string
	ClientID         string
	ConnID           string // the status-stream connection carrying the frames
	Tools            []utils.FunctionToolsRequest
	ClientFolderPath string
	RegisteredAt     time.Time
	ExpiresAt        time.Time
}

func (c *deviceClaim) live(now time.Time) bool {
	return c != nil && now.Before(c.ExpiresAt)
}

// devicePending is one device tool call waiting for its result.
type devicePending struct {
	ID             string
	UserID         string
	ConversationID string
	ToolCallID     string
	Name           string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	// results is buffered (1) so Resolve() never blocks on a consumer that has
	// already walked away (timeout, cancellation, client disconnect).
	results chan deviceResult
}

type deviceResult struct {
	Result string
	Err    string
}

func (p *devicePending) live(now time.Time) bool { return now.Before(p.ExpiresAt) }

// DeviceRegistry is the process-wide channel. Like StatusRegistry it is a
// global: claims and pending calls are in-memory state tied to live
// connections, and a server restart drops both (the app re-claims on
// reconnect).
var DeviceRegistry = &deviceRegistry{
	claims:  make(map[string]*deviceClaim),
	pending: make(map[string]*devicePending),
}

// Device claim TTL bounds, per docs/device-channel.md: default 120s, clamped
// to 15..600s. The app re-claims every ~30s while its status stream is open,
// so the default gives ~4 renewals of headroom: an app whose connection dies
// stops refreshing and its claim lapses within the TTL, which is exactly what
// keeps a background run from dispatching to a dead device and hanging.
const (
	deviceClaimTTLDefault = 120 * time.Second
	deviceClaimTTLMin     = 15 * time.Second
	deviceClaimTTLMax     = 600 * time.Second
)

// deviceToolTimeout is how long a device-channel call waits for the app to
// POST its result before the loop synthesizes an error and arms the
// device-unresponsive memo. Longer than the chat path's stall watchdog
// (clientToolStallTimeout, 90s) because the call arrives unattended: there is
// no user watching the app who can answer an approval prompt, and the device
// executor may legitimately be grinding a long command.
const deviceToolTimeout = 180 * time.Second

// clampDeviceClaimTTL normalizes a client-supplied TTL to the documented
// window. Zero / negative means "use the default".
func clampDeviceClaimTTL(seconds int) time.Duration {
	if seconds <= 0 {
		return deviceClaimTTLDefault
	}
	d := time.Duration(seconds) * time.Second
	if d < deviceClaimTTLMin {
		return deviceClaimTTLMin
	}
	if d > deviceClaimTTLMax {
		return deviceClaimTTLMax
	}
	return d
}

// Claim stores (or refreshes) the app's advertisement for a user and returns
// the new expiry.
//
// One active claim per user, freshest wins: two devices logged in as the same
// user can both claim, and the one that refreshed last owns the work. A claim
// from a connection that is still refreshing must never be displaced by one
// that stopped (its ExpiresAt is in the past, so it already lost).
//
// Tools are sorted and de-duplicated by name AT STORE TIME. The list is fed
// into payload.ClientSideTools by the jobs runner and then rendered into the
// prompt prefix by ai_tools.GetRequests — Go randomizes map iteration order,
// so an unsorted claim list would byte-shuffle the tools array on every turn
// and collapse KV prompt-cache reuse (the incident documented in
// ai_tools/registry.go: prefix reuse pinned to the 16384-token checkpoint
// floor, 40-70k tokens re-prefilled per turn). Sorting here is the performance
// contract, not cosmetics.
func (r *deviceRegistry) Claim(userID, clientID, connID, clientFolderPath string, tools []utils.FunctionToolsRequest, ttl time.Duration) time.Time {
	if userID == "" || clientID == "" {
		return time.Time{}
	}
	now := time.Now()
	expires := now.Add(ttl)
	normalized := normalizeClientSideTools(tools)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.purgeExpiredClaimsLocked(now)

	prev := r.claims[userID]
	if prev != nil && prev.ClientID != clientID && prev.live(now) && prev.RegisteredAt.After(now) {
		// A claim stamped in the future (clock skew) from a DIFFERENT client
		// keeps the slot for the rest of its window; do not clobber it.
		return prev.ExpiresAt
	}
	r.claims[userID] = &deviceClaim{
		UserID:           userID,
		ClientID:         clientID,
		ConnID:           connID,
		Tools:            normalized,
		ClientFolderPath: clientFolderPath,
		RegisteredAt:     now,
		ExpiresAt:        expires,
	}
	return expires
}

// ClaimOnConnect registers the placeholder claim for a freshly connected status
// stream that carries a client_id. It advertises NO tools: the app's real tool
// list arrives through POST /client/claim. The placeholder exists so the
// connection identity is known (ClientIDForUser) and so the claim slot is tied
// to the connection from the first byte — DropForConn removes it when the
// stream closes, per the spec: "the claim lives exactly as long as its
// status-stream connection".
func (r *deviceRegistry) ClaimOnConnect(userID, clientID, connID string) {
	if userID == "" || clientID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.purgeExpiredClaimsLocked(now)
	prev := r.claims[userID]
	// Never downgrade a claim that already advertises tools: a reconnect from
	// the same client must not wipe the advertisement it posted seconds ago.
	if prev != nil && prev.ClientID == clientID && prev.live(now) {
		prev.ConnID = connID
		return
	}
	if prev != nil && prev.ClientID != clientID && prev.live(now) && prev.RegisteredAt.After(now) {
		return
	}
	r.claims[userID] = &deviceClaim{
		UserID:       userID,
		ClientID:     clientID,
		ConnID:       connID,
		RegisteredAt: now,
		ExpiresAt:    now.Add(deviceClaimTTLDefault),
	}
}

// DropForConn drops the claim owned by a status-stream connection, which is
// how a claim dies with its connection. A claim refreshed by a NEWER
// connection is left alone (freshest wins), so a reconnect race — the old
// socket's Remove arriving after the new socket claimed — cannot blind the
// user's background runs.
func (r *deviceRegistry) DropForConn(connID string) {
	if connID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for userID, c := range r.claims {
		if c.ConnID == connID {
			delete(r.claims, userID)
		}
	}
}

// ClaimForUser returns the user's live claim, or nil when there is none or it
// lapsed. Expiry is checked lazily on read (and stale entries evicted there),
// so a device that stopped refreshing is invisible within one TTL with no
// background sweeper: a background run then hits the existing fast-fail path
// instead of dispatching into a dead app and hanging.
func (r *deviceRegistry) ClaimForUser(userID string) *deviceClaim {
	if userID == "" {
		return nil
	}
	now := time.Now()
	r.mu.RLock()
	c := r.claims[userID]
	r.mu.RUnlock()
	if c == nil {
		return nil
	}
	if c.live(now) {
		return c
	}
	r.mu.Lock()
	// Re-check under the write lock: a refresh may have landed between the
	// read and the upgrade, and must not be evicted.
	if cur := r.claims[userID]; cur == c {
		delete(r.claims, userID)
	}
	r.mu.Unlock()
	return nil
}

// ClaimedTool reports whether the user's live claim advertises this tool. The
// device channel only ever dispatches tools the claim advertises: the app
// refuses anything else, and routing a call the device cannot run would burn
// the dispatch timeout for nothing.
func (r *deviceRegistry) ClaimedTool(name, userID string) bool {
	c := r.ClaimForUser(userID)
	if c == nil || name == "" {
		return false
	}
	for _, t := range c.Tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// ToolsForUser returns the live claim's advertised tool definitions, sorted by
// name, for a background run to put in ChatPayload.ClientSideTools. Returns
// nil when the user has no live claim — the run then advertises no device
// tools, which is the pre-existing behaviour, and the fast-fail error text
// stays exactly as today.
func (r *deviceRegistry) ToolsForUser(userID string) []utils.FunctionToolsRequest {
	c := r.ClaimForUser(userID)
	if c == nil || len(c.Tools) == 0 {
		return nil
	}
	// Copy: the claim is replaced by later re-claims, and callers hand this
	// slice to the request builder.
	out := make([]utils.FunctionToolsRequest, len(c.Tools))
	copy(out, c.Tools)
	return out
}

// ClientFolderPathForUser returns the claim's attached device folder, "" when
// none. Opaque to the server (see utils.ModelSelected.ClientFolderPath); the
// device channel reports it so a background run can tell the model what the
// device can reach.
func (r *deviceRegistry) ClientFolderPathForUser(userID string) string {
	c := r.ClaimForUser(userID)
	if c == nil {
		return ""
	}
	return c.ClientFolderPath
}

// HasLiveClaim reports whether the user has a live claim. This is the term the
// tool loop's reachability decision consults.
func (r *deviceRegistry) HasLiveClaim(userID string) bool {
	return r.ClaimForUser(userID) != nil
}

// --- Pending device tool requests ---

// RegisterPending records a dispatched device tool call and returns the
// channel its result arrives on plus a cleanup func the caller MUST defer
// (Remove), so the map cannot accumulate calls nobody is waiting for.
func (r *deviceRegistry) RegisterPending(requestID, userID, conversationID, toolCallID, name string, timeout time.Duration) (<-chan deviceResult, func()) {
	now := time.Now()
	p := &devicePending{
		ID:             requestID,
		UserID:         userID,
		ConversationID: conversationID,
		ToolCallID:     toolCallID,
		Name:           name,
		CreatedAt:      now,
		ExpiresAt:      now.Add(timeout),
		results:        make(chan deviceResult, 1),
	}
	r.mu.Lock()
	r.purgeExpiredPendingLocked(now)
	r.pending[requestID] = p
	r.mu.Unlock()
	return p.results, func() { r.Remove(requestID) }
}

// Resolve delivers the device's answer for a pending request. Returns false
// when the request is unknown, already answered, expired, or belongs to a
// different user (the caller answers 404; the app ignores the response). The
// userID check is the authorization: an app may only answer calls it was asked
// to run, never another user's.
func (r *deviceRegistry) Resolve(requestID, userID, toolCallID, name, result, errText string) bool {
	now := time.Now()
	r.mu.Lock()
	p := r.pending[requestID]
	if p == nil || !p.live(now) {
		if p != nil {
			delete(r.pending, requestID)
		}
		r.purgeExpiredPendingLocked(now)
		r.mu.Unlock()
		return false
	}
	if userID == "" || p.UserID != userID {
		r.mu.Unlock()
		return false
	}
	// Best-effort identity check: the app echoes back what the frame carried.
	// A mismatch is a client bug (or a stale frame it replayed), and answering
	// a call the app is not finishing would double-resolve a live turn.
	if toolCallID != "" && p.ToolCallID != "" && toolCallID != p.ToolCallID {
		r.mu.Unlock()
		return false
	}
	if name != "" && p.Name != "" && name != p.Name {
		r.mu.Unlock()
		return false
	}
	delete(r.pending, requestID)
	r.mu.Unlock()

	// results is buffered, so delivering never blocks on the waiting loop.
	p.results <- deviceResult{Result: result, Err: errText}
	return true
}

// Remove drops a pending request (the caller's deferred cleanup). Safe to call
// after Resolve (already gone) and from any goroutine.
func (r *deviceRegistry) Remove(requestID string) {
	if requestID == "" {
		return
	}
	r.mu.Lock()
	delete(r.pending, requestID)
	r.mu.Unlock()
}

// PendingCount is the number of pending requests held. Diagnostic / test hook.
func (r *deviceRegistry) PendingCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

// ClaimCount is the number of claims held (live or lapsed). Diagnostic / test hook.
func (r *deviceRegistry) ClaimCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.claims)
}

// Reset clears all state. Tests only.
func (r *deviceRegistry) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.claims = make(map[string]*deviceClaim)
	r.pending = make(map[string]*devicePending)
}

// purgeExpiredClaimsLocked evicts lapsed claims. Called opportunistically on
// writes so the maps stay bounded by the number of live connections.
func (r *deviceRegistry) purgeExpiredClaimsLocked(now time.Time) {
	for userID, c := range r.claims {
		if !c.live(now) {
			delete(r.claims, userID)
		}
	}
}

// purgeExpiredPendingLocked evicts pending calls whose dispatch deadline has
// passed: the loop already synthesized a result for them, so a late POST must
// be rejected, not delivered into a turn that moved on.
func (r *deviceRegistry) purgeExpiredPendingLocked(now time.Time) {
	for id, p := range r.pending {
		if !p.live(now) {
			delete(r.pending, id)
		}
	}
}

// normalizeClientSideTools sorts tool definitions by name and drops duplicate
// names (first occurrence wins), returning nil for an empty result.
// Deterministic ordering is a hard contract: these definitions are rendered
// into the prompt prefix (see Claim).
func normalizeClientSideTools(tools []utils.FunctionToolsRequest) []utils.FunctionToolsRequest {
	if len(tools) == 0 {
		return nil
	}
	out := make([]utils.FunctionToolsRequest, 0, len(tools))
	seen := make(map[string]bool, len(tools))
	for _, t := range tools {
		if t.Name == "" || seen[t.Name] {
			continue
		}
		seen[t.Name] = true
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) == 0 {
		return nil
	}
	return out
}

// IntersectClientSideTools returns tools restricted to the names present in
// allowed, sorted by name and de-duplicated. When allowed is empty it is
// treated as "no restriction known" and tools is returned normalized.
//
// Used for sub-agents: a sub-agent can never gain device tools its parent did
// not advertise. Consent beyond that is enforced by ai_tools.GetRequests,
// which filters client tools against model.Tools using the namespaced key, so
// the preset's tool config stays the gate.
func IntersectClientSideTools(tools, allowed []utils.FunctionToolsRequest) []utils.FunctionToolsRequest {
	if len(tools) == 0 {
		return nil
	}
	if len(allowed) == 0 {
		return normalizeClientSideTools(tools)
	}
	names := make(map[string]bool, len(allowed))
	for _, t := range allowed {
		names[t.Name] = true
	}
	var out []utils.FunctionToolsRequest
	for _, t := range tools {
		if names[t.Name] {
			out = append(out, t)
		}
	}
	return normalizeClientSideTools(out)
}

// combineDeviceToolResult concatenates the device's answer the way a chat tool
// result is stored: output and error joined by a newline when both are
// present. Keeps the device channel and the chat POST path byte-identical, so
// the model sees the same shape whichever route answered.
func combineDeviceToolResult(result, errText string) string {
	switch {
	case result == "":
		return errText
	case errText == "":
		return result
	default:
		return result + "\n" + errText
	}
}
