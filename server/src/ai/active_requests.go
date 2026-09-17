package ai

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"github.com/azukaar/plurality/src/utils"
)

// SSEClient represents a single connected SSE listener.
// sendMu serializes writes to the underlying ResponseWriter: net/http is not
// safe for concurrent writes on the same connection, and Broadcast snapshots
// the client set so two Broadcast calls (mid-turn "text", end-turn "done")
// can otherwise interleave frames on one socket.
type SSEClient struct {
	writer  http.ResponseWriter
	flusher http.Flusher
	Done    chan struct{}
	sendMu  sync.Mutex
}

// NewSSEClient creates an SSEClient from an HTTP response writer.
// Returns nil if the writer does not support flushing.
func NewSSEClient(w http.ResponseWriter) *SSEClient {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil
	}
	return &SSEClient{
		writer:  w,
		flusher: flusher,
		Done:    make(chan struct{}),
	}
}

// Send writes an SSEEvent to this client. Returns false if the write fails.
// It does not hold any registry lock and never spawns a goroutine: a broken
// socket surfaces as a failed write (WriteSSEEvent recovers
// http.ErrAbortHandler — see sse_events.go), so a client disconnect cannot
// panic the process nor wedge callers on a forever-blocking Flush.
func (c *SSEClient) Send(event SSEEvent) bool {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return WriteSSEEvent(c.writer, event) == nil
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
}

// RemoveClient unregisters an SSE client.
func (ar *ActiveRequest) RemoveClient(client *SSEClient) {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	delete(ar.clients, client)
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
	// Snapshot the client set under the lock, then send WITHOUT holding
	// ar.mu. The old code wrote to clients while holding ar.mu: if one SSE
	// socket was dead-but-open, ResponseWriter.Flush() blocked forever,
	// wedging ar.mu. Then every AddClient/RemoveClient/CloseAllClients
	// (cleanup at the end of a turn) blocked too — the UI stayed up (SPA)
	// but chats wouldn't load or start, exactly the reported symptom, until
	// a watchdog killed the container. This fires at 'No tool calls,
	// setting idle and broadcasting done' — the conversation-end teardown.
	ar.mu.RLock()
	clients := make([]*SSEClient, 0, len(ar.clients))
	for c := range ar.clients {
		clients = append(clients, c)
	}
	ar.mu.RUnlock()

	if len(clients) == 0 && event.Type != "text" {
		utils.Debug("[Broadcast] No clients connected for %s event on %s", event.Type, ar.ConversationID)
	}

	for _, client := range clients {
		// Send outside the lock. A broken socket surfaces as a failed
		// write (WriteSSEEvent recovers ErrAbortHandler), so we drop the
		// client here instead of letting it wedge the registry on a
		// forever-blocking Flush. No per-event goroutine is spawned: an
		// uncancellable goroutine per event would leak forever on a stuck
		// socket and could write to the same connection concurrently with
		// the next Broadcast (sendMu prevents frame interleaving, not the
		// leak).
		if !client.Send(event) {
			ar.mu.Lock()
			if _, ok := ar.clients[client]; ok {
				delete(ar.clients, client)
				select {
				case <-client.Done:
				default:
					close(client.Done)
				}
			}
			ar.mu.Unlock()
		}
	}
}

// CloseAllClients signals all connected clients that the stream is done.
func (ar *ActiveRequest) CloseAllClients() {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	for client := range ar.clients {
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
type StatusClient struct {
	writer  http.ResponseWriter
	flusher http.Flusher
	UserID  string
	Done    chan struct{}
}

// Send writes a StatusEvent to this client. Returns false if the write fails.
func (c *StatusClient) Send(event StatusEvent) bool {
	return WriteStatusEvent(c.writer, event) == nil
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
}

// Remove unregisters a status stream client.
func (r *statusRegistry) Remove(client *StatusClient) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.clients, client)
}

// BroadcastToUser sends a StatusEvent to all status clients belonging to a
// specific user. The client set is snapshotted under the lock and sent
// WITHOUT holding r.mu: the old code wrote to clients while holding the
// global status mutex, so one dead-but-open status socket (whose
// ResponseWriter.Flush blocks forever) wedged EVERY BroadcastToUser and every
// Add/Remove of the status stream — the whole backend froze, the SPA stayed
// up but loaded nothing, and a watchdog eventually killed the container.
func (r *statusRegistry) BroadcastToUser(userID string, event StatusEvent) {
	r.mu.RLock()
	clients := make([]*StatusClient, 0, len(r.clients))
	for client := range r.clients {
		if client.UserID == userID {
			clients = append(clients, client)
		}
	}
	r.mu.RUnlock()

	for _, client := range clients {
		if !client.Send(event) {
			r.mu.Lock()
			if _, ok := r.clients[client]; ok {
				delete(r.clients, client)
				select {
				case <-client.Done:
				default:
					close(client.Done)
				}
			}
			r.mu.Unlock()
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