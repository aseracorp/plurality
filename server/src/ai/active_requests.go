package ai

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

// sseHeartbeatInterval is how often idle SSE clients receive a keepalive
// comment. Browsers (Firefox especially) throttle or drop long-lived fetch
// streams in background tabs; without periodic bytes the connection looks
// dead and the tab comes back to a frozen chat. A comment line is invisible
// to SSE parsers (only "data:" lines are read) and keeps proxies from
// buffering the stream away.
const sseHeartbeatInterval = 25 * time.Second

// SSEClient represents a single connected SSE listener.
//
// Writes to the underlying ResponseWriter only ever happen on the dedicated
// writer goroutine (see startWriter). Send() merely enqueues into a bounded
// channel and returns; it never touches the socket. This is the core of the
// fix for "workflows stop and need a restart": a client that stopped reading
// fills its TCP buffer, and a blocking Flush() inside the LLM loop goroutine
// froze the whole conversation (observed with goroutine 17117 stuck in Flush
// for 4+ minutes). With the writer goroutine, the LLM loop can never block on
// a slow or dead client — at worst the writer goroutine stalls, the queue
// fills, Send() returns false, and Broadcast() drops the client.
type SSEClient struct {
	writer  http.ResponseWriter
	flusher http.Flusher
	Done    chan struct{}

	// outQ is a bounded queue of SSE events to write. Send() enqueues;
	// the writer goroutine drains and flushes. Bounded so a stalled writer
	// (blocked flush) back-pressures fast: once full, new events are
	// rejected and the client is dropped instead of queueing unboundedly.
	outQ chan SSEEvent

	writeMu       sync.Mutex // serializes direct writer access (start/stop)
	heartbeatStop chan struct{}
	heartbeatOnce sync.Once
}

// NewSSEClient creates an SSEClient from an HTTP response writer.
// Returns nil if the writer does not support flushing.
// sseOutQueueDepth bounds the per-client pending write queue. Deep enough
// that a healthy stream (LLM tokens arrive roughly as fast as they can be
// flushed) never drops events; shallow enough that a stalled client is
// detected and dropped within a few hundred KB of queued bytes.
const sseOutQueueDepth = 512

func NewSSEClient(w http.ResponseWriter) *SSEClient {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil
	}
	return &SSEClient{
		writer:        w,
		flusher:       flusher,
		Done:          make(chan struct{}),
		outQ:          make(chan SSEEvent, sseOutQueueDepth),
		heartbeatStop: make(chan struct{}),
	}
}

// Send enqueues an SSE event for this client and returns without touching
// the socket. Returns false when the client is gone (Done closed) or the
// bounded queue is full (the writer goroutine is stalled behind a blocked
// flush — i.e. the client stopped reading), in which case the caller
// (Broadcast) drops the client. Send is safe to call from the LLM loop
// goroutine: it can never block on network I/O.
func (c *SSEClient) Send(event SSEEvent) bool {
	if c.outQ == nil {
		return false // struct-literal client without a writer
	}
	select {
	case <-c.Done:
		return false
	default:
	}
	select {
	case c.outQ <- event:
		return true
	default:
		// Queue full: writer is behind. Drop as stalled.
		close(c.Done)
		return false
	}
}

// startWriter launches the single goroutine that owns all writes to this
// client's ResponseWriter. It drains outQ, writing each event and flushing,
// and sends ": ping" keepalives whenever the queue idles for
// sseHeartbeatInterval. All socket I/O for this client happens here and only
// here — Send() never touches the socket, so a stalled client (blocked
// flush) cannot stall the LLM loop that called Send. If the writer itself
// blocks, the queue eventually fills and Send drops the client.
//
// The goroutine is fully panic-safe: a late write or flush on a response
// whose HTTP handler has already returned (which net/http finalizes by
// nilling its internal buffer) must never take down the whole process, so
// any panic is recovered and the loop exits.
func (c *SSEClient) startWriter() {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				utils.Log("[SSEClient] writer goroutine recovered from panic: %v", r)
			}
		}()
		ticker := time.NewTicker(sseHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-c.Done:
				return
			case <-c.heartbeatStop:
				return
			case ev := <-c.outQ:
				// A healthy client drains as fast as we enqueue, so the
				// queue is almost always empty here and the next select
				// immediately picks up the ping deadline.
				c.writeMu.Lock()
				setSSEWriteDeadline(c.writer)
				err := WriteSSEEvent(c.writer, ev)
				clearSSEWriteDeadline(c.writer)
				c.writeMu.Unlock()
				if err != nil {
					close(c.Done)
					return
				}
			case <-ticker.C:
				if !c.writeHeartbeat() {
					close(c.Done)
					return
				}
			}
		}
	}()
}

// writeHeartbeat writes a single keepalive comment. Only called from the
// writer goroutine (which owns the socket writes), so no lock is needed.
// Returns false if the write fails (client is gone).
func (c *SSEClient) writeHeartbeat() bool {
	select {
	case <-c.Done:
		return false
	default:
	}
	setSSEWriteDeadline(c.writer)
	_, err := c.writer.Write([]byte(": ping\n\n"))
	clearSSEWriteDeadline(c.writer)
	if err != nil {
		return false
	}
	c.flusher.Flush()
	return true
}

// stopWriter idempotently stops the writer goroutine.
func (c *SSEClient) stopWriter() {
	// Guard against struct-literal clients that never initialized the
	// channel (see NewSSEClient): closing a nil channel panics, and this
	// can be called from the LLM loop goroutine where a panic would kill
	// a live conversation.
	if c.heartbeatStop == nil {
		return
	}
	c.heartbeatOnce.Do(func() {
		close(c.heartbeatStop)
	})
}

// ActiveRequest tracks an in-progress LLM request for a conversation.
// The LLM loop goroutine writes to this; SSE clients subscribe for events.
type ActiveRequest struct {
	ConversationID string
	UserID         string
	Ctx            context.Context
	Cancel         context.CancelFunc
	State          utils.ConversationState
	Model          utils.Model
	ModelSelected  utils.ModelSelected

	mu      sync.RWMutex
	clients map[*SSEClient]bool

	// In-memory buffer for the current assistant turn
	TextBuffer       strings.Builder
	ToolCallBuffer   []utils.ToolCall
	TokenUsage       int
	PromptTokens     int
	CompletionTokens int
	ResponseCost     float64
}

// NewActiveRequest creates a new ActiveRequest with a cancelable context.
func NewActiveRequest(conversationID string, userID string, model utils.Model, modelSelected utils.ModelSelected) *ActiveRequest {
	ctx, cancel := context.WithCancel(context.Background())
	return &ActiveRequest{
		ConversationID: conversationID,
		UserID:         userID,
		Ctx:            ctx,
		Cancel:         cancel,
		State:          utils.StateProcessing,
		Model:          model,
		ModelSelected:  modelSelected,
		clients:        make(map[*SSEClient]bool),
	}
}

// AddClient registers an SSE client to receive broadcast events.
func (ar *ActiveRequest) AddClient(client *SSEClient) {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	ar.clients[client] = true
	client.startWriter()
}

// RemoveClient unregisters an SSE client.
func (ar *ActiveRequest) RemoveClient(client *SSEClient) {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	delete(ar.clients, client)
	client.stopWriter()
}

// ClientCount returns the number of connected SSE clients.
func (ar *ActiveRequest) ClientCount() int {
	ar.mu.RLock()
	defer ar.mu.RUnlock()
	return len(ar.clients)
}

// Broadcast sends an SSEEvent to all connected clients.
// Disconnected clients are automatically removed.
func (ar *ActiveRequest) Broadcast(event SSEEvent) {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	if len(ar.clients) == 0 && event.Type != "text" {
		utils.Debug("[Broadcast] No clients connected for %s event on %s", event.Type, ar.ConversationID)
	}
	for client := range ar.clients {
		if !client.Send(event) {
			delete(ar.clients, client)
			client.stopWriter()
			select {
			case <-client.Done:
				// already closed
			default:
				close(client.Done)
			}
		}
	}
}

// CloseAllClients signals all connected clients that the stream is done.
func (ar *ActiveRequest) CloseAllClients() {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	for client := range ar.clients {
		client.stopWriter()
		select {
		case <-client.Done:
			// already closed
		default:
			close(client.Done)
		}
		delete(ar.clients, client)
	}
}

// BroadcastStatus sends a compact status event to all status stream clients
// for this user. The conversation's current ModelSelected snapshot is
// included so any client with the sidebar open — not just the per-conversation
// SSE viewer — picks up lock / folder / eco / model changes live.
func (ar *ActiveRequest) BroadcastStatus(activity string, toolName string) {
	msSnap := ar.ModelSelected
	StatusRegistry.BroadcastToUser(ar.UserID, StatusEvent{
		ConversationID: ar.ConversationID,
		State:          string(ar.State),
		Activity:       activity,
		ToolName:       toolName,
		ModelSelected:  &msSnap,
	})
}

// ResetBuffer clears the in-memory text and tool call buffers for a new LLM turn.
func (ar *ActiveRequest) ResetBuffer() {
	ar.TextBuffer.Reset()
	ar.ToolCallBuffer = nil
	ar.TokenUsage = 0
	ar.PromptTokens = 0
	ar.CompletionTokens = 0
	ar.ResponseCost = 0
}

// --- Global Registry ---

// ActiveRequestRegistry manages all in-progress requests across conversations.
var RequestRegistry = &activeRequestRegistry{
	requests: make(map[string]*ActiveRequest),
}

type activeRequestRegistry struct {
	mu       sync.RWMutex
	requests map[string]*ActiveRequest
}

// Get returns the ActiveRequest for a conversation, or nil if none exists.
func (r *activeRequestRegistry) Get(conversationID string) *ActiveRequest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.requests[conversationID]
}

// Set registers an ActiveRequest for a conversation.
func (r *activeRequestRegistry) Set(conversationID string, ar *ActiveRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests[conversationID] = ar
}

// SetIfAbsent registers an ActiveRequest only if none is currently registered
// for the conversation. Returns true when this call won the slot. Used by the
// client-tool stall watchdog to atomically claim the resume — two watchdogs
// racing for the same conversation can never both relaunch the loop.
func (r *activeRequestRegistry) SetIfAbsent(conversationID string, ar *ActiveRequest) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.requests[conversationID] != nil {
		return false
	}
	r.requests[conversationID] = ar
	return true
}

// Remove unregisters an ActiveRequest for a conversation.
func (r *activeRequestRegistry) Remove(conversationID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.requests, conversationID)
}

// GetForUser returns all ActiveRequests belonging to a specific user.
func (r *activeRequestRegistry) GetForUser(userID string) []*ActiveRequest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []*ActiveRequest
	for _, ar := range r.requests {
		if ar.UserID == userID {
			result = append(result, ar)
		}
	}
	return result
}

// --- Global Status Stream ---

// StatusClient is a connected listener on the global status stream.
// Same writer-goroutine design as SSEClient: Send() enqueues into a bounded
// channel; a dedicated writer goroutine owns all socket writes so a stalled
// status client can never freeze BroadcastStatus inside the LLM loop.
type StatusClient struct {
	writer  http.ResponseWriter
	flusher http.Flusher
	UserID  string
	Done    chan struct{}

	outQ          chan StatusEvent
	writeMu       sync.Mutex
	heartbeatStop chan struct{}
	heartbeatOnce sync.Once
}

// NewStatusClient creates a StatusClient.
func NewStatusClient(w http.ResponseWriter, flusher http.Flusher, userID string) *StatusClient {
	return &StatusClient{
		writer:        w,
		flusher:       flusher,
		UserID:        userID,
		Done:          make(chan struct{}),
		outQ:          make(chan StatusEvent, sseOutQueueDepth),
		heartbeatStop: make(chan struct{}),
	}
}

// Send writes a StatusEvent to this client. Returns false if the write fails.
// Bounded by sseWriteTimeout like SSEClient.Send — a status stream client
// that stopped reading must not block BroadcastStatus inside the LLM loop.
func (c *StatusClient) Send(event StatusEvent) bool {
	if c.outQ == nil {
		return false
	}
	select {
	case <-c.Done:
		return false
	default:
	}
	select {
	case c.outQ <- event:
		return true
	default:
		close(c.Done)
		return false
	}
}

// writeHeartbeat writes a single keepalive comment under the write lock.
// It returns false if the write fails or if the client has been torn down.
func (c *StatusClient) writeHeartbeat() bool {
	select {
	case <-c.Done:
		return false
	default:
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	setSSEWriteDeadline(c.writer)
	_, err := c.writer.Write([]byte(": ping\n\n"))
	clearSSEWriteDeadline(c.writer)
	if err != nil {
		return false
	}
	c.flusher.Flush()
	return true
}

// startWriter launches the writer goroutine for this client: drains outQ
// and sends keepalives. Panic-safe: a flush on a finalized response must
// never crash the process.
func (c *StatusClient) startWriter() {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				utils.Log("[StatusClient] writer goroutine recovered from panic: %v", r)
			}
		}()
		ticker := time.NewTicker(sseHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-c.Done:
				return
			case <-c.heartbeatStop:
				return
			case ev := <-c.outQ:
				c.writeMu.Lock()
				setSSEWriteDeadline(c.writer)
				err := WriteStatusEvent(c.writer, ev)
				clearSSEWriteDeadline(c.writer)
				c.writeMu.Unlock()
				if err != nil {
					close(c.Done)
					return
				}
			case <-ticker.C:
				if !c.writeHeartbeat() {
					close(c.Done)
					return
				}
			}
		}
	}()
}

// stopWriter idempotently stops the writer goroutine.
func (c *StatusClient) stopWriter() {
	// Guard against struct-literal clients that never initialized the
	// channel (handleStatusStream used to build one): closing a nil
	// channel panics — and when this runs inside the LLM loop goroutine
	// (StatusRegistry.BroadcastToUser → remove → stopWriter) that panic
	// kills the conversation's loop mid-turn, leaving it stuck in
	// "processing" forever.
	if c.heartbeatStop == nil {
		return
	}
	c.heartbeatOnce.Do(func() {
		close(c.heartbeatStop)
	})
}

// StatusRegistry manages all connected status stream clients.
var StatusRegistry = &statusRegistry{
	clients: make(map[*StatusClient]bool),
}

type statusRegistry struct {
	mu      sync.RWMutex
	clients map[*StatusClient]bool
}

// Add registers a status stream client.
func (r *statusRegistry) Add(client *StatusClient) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clients[client] = true
	client.startWriter()
}

// Remove unregisters a status stream client.
func (r *statusRegistry) Remove(client *StatusClient) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.clients, client)
	client.stopWriter()
}

// BroadcastToUser sends a StatusEvent to all status clients belonging to a specific user.
func (r *statusRegistry) BroadcastToUser(userID string, event StatusEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for client := range r.clients {
		if client.UserID == userID {
			if !client.Send(event) {
				delete(r.clients, client)
				client.stopWriter()
				select {
				case <-client.Done:
					// already closed
				default:
					close(client.Done)
				}
			}
		}
	}
}

// CopyUserContext creates a context that carries the userID from the original HTTP
// request, but is NOT canceled when that request ends. Used for goroutines that
// outlive the HTTP connection.
func CopyUserContext(r *http.Request) context.Context {
	userID, _ := r.Context().Value("userID").(string)
	return context.WithValue(context.Background(), "userID", userID)
}

// DefaultSSEWriteTimeout is the exported, documented default for the SSE
// write bound (see sseWriteTimeout).
const DefaultSSEWriteTimeout = 2 * time.Second

// sseWriteTimeout bounds a single SSE write to a client. It is deliberately
// short: a healthy client flushes in microseconds, and anything slower is a
// dead or stalled pipe that must be dropped rather than allowed to freeze the
// LLM loop. Keepalive pings and status events share the same bound. It is a
// var (not a const) so tests can shrink it and prove the bound.
var sseWriteTimeout = DefaultSSEWriteTimeout

// setSSEWriteDeadline arms a write deadline on the ResponseWriter backing an
// SSE client. The Go net/http server writer implements SetWriteDeadline
// directly (it schedules a stream write timeout that closes the connection:
// net/http server.go, responseWriter.SetWriteDeadline) — the *correct* way to
// bound a blocking Flush. http.NewResponseController does NOT expose that
// method (it only supports h2c-style Hijack conns), so we probe the writer's
// interface first and fall back to the controller (a no-op there, but at
// least harmless). For writers with neither, writes stay synchronous.
func setSSEWriteDeadline(w http.ResponseWriter) {
	if dw, ok := w.(interface{ SetWriteDeadline(t time.Time) error }); ok {
		_ = dw.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
}

// clearSSEWriteDeadline removes the write deadline after a successful write
// so the next write (possibly much later, e.g. the next heartbeat) is not
// spuriously timed out. No-op for writers without deadline support.
func clearSSEWriteDeadline(w http.ResponseWriter) {
	if dw, ok := w.(interface{ SetWriteDeadline(t time.Time) error }); ok {
		_ = dw.SetWriteDeadline(time.Time{})
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
}
