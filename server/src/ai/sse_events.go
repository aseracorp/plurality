package ai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

// sseWriteTimeout bounds each individual SSE frame write+flush. The HTTP
// server runs with no WriteTimeout (http.ListenAndServe), so a dead-but-open
// client socket can make ResponseWriter.Flush() block forever. That used to
// wedge Broadcast (and with it the LLM loop) whenever a client vanished at
// the worst moment. Setting a per-frame deadline synchronously — no
// goroutine, no leak — makes a stuck socket fail the write instead of
// hanging the turn.
const sseWriteTimeout = 10 * time.Second

func setWriteDeadline(w http.ResponseWriter) func() {
	rc := http.NewResponseController(w)
	// Best effort: if the ResponseWriter does not support deadlines this is
	// a no-op and the previous behavior applies (socket normally fails fast).
	_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
	return func() {
		_ = rc.SetWriteDeadline(time.Time{})
	}
}

// SSEEvent is the unified event type streamed to clients over SSE.
type SSEEvent struct {
	Type           string          `json:"type"`                   // "text", "tool_use", "tool_result", "state_change", "done", "error"
	Content        string          `json:"content,omitempty"`      // text chunk for "text" events
	ToolCall       *utils.ToolCall `json:"tool_call,omitempty"`    // for "tool_use" events
	ToolCallID     string          `json:"tool_call_id,omitempty"` // for "tool_result" events
	ToolName       string          `json:"tool_name,omitempty"`    // for "tool_result" events
	ToolResult     string          `json:"tool_result,omitempty"`  // for "tool_result" events
	IsServer       bool            `json:"is_server"`              // true = server-side tool, false = client must execute
	ConversationID string          `json:"conversation_id"`
	State          string          `json:"state,omitempty"` // for "state_change" events
	Model            *utils.Model `json:"model,omitempty"`
	TotalTokens      int          `json:"total_tokens,omitempty"`
	PromptTokens     int          `json:"prompt_tokens,omitempty"`
	CompletionTokens int          `json:"completion_tokens,omitempty"`
	ResponseCost     float64      `json:"response_cost,omitempty"`
	Title            string       `json:"title,omitempty"`

	// ModelSelected is the conversation's current per-conversation settings
	// snapshot — tools, eco mode, attached folder, and client lock. Populated
	// on "tool_use" events (so other connected clients see the lock holder
	// before they'd race to execute the tool) and on "done" events (so
	// folder / eco / tool / model swaps round-trip to every viewer).
	ModelSelected *utils.ModelSelected `json:"model_selected,omitempty"`
}

// WriteSSEEvent serializes an SSEEvent and writes it to an HTTP response writer.
func WriteSSEEvent(w http.ResponseWriter, event SSEEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}

	// Writing to an SSE connection whose client has disconnected panics with
	// http.ErrAbortHandler (a fatal in Go's net/http that is not auto-recovered
	// in a streaming goroutine). This happens at the most painful time — right
	// after the final "done" event is emitted, when the user has typically
	// navigated away or the tab/connection dropped. An uncaught ErrAbortHandler
	// kills the whole process. Recover it here so a dead client can never take
	// down the server.
	defer func() {
		if r := recover(); r != nil {
			// http.ErrAbortHandler is the expected "client went away" case.
			utils.Log("[SSE] client connection aborted during write: %v", r)
		}
	}()

	restore := setWriteDeadline(w)
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	if err != nil {
		restore()
		return err
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	restore()
	return nil
}

// StatusEvent is a lightweight event for the global status stream.
// No content, no tool args, no results — just enough for UI indicators.
type StatusEvent struct {
	ConversationID string `json:"conversation_id"`
	State          string `json:"state"`               // "processing", "idle", "waiting_for_tool"
	Activity       string `json:"activity,omitempty"`  // "typing", "tool_use"
	ToolName       string `json:"tool_name,omitempty"` // only when activity is "tool_use"
	Title          string `json:"title,omitempty"`     // set when title is generated server-side
	Icon           string `json:"icon,omitempty"`      // set when icon is generated server-side

	// ModelSelected is the conversation's current per-conversation settings
	// snapshot, broadcast on the global status stream so any client with the
	// sidebar open (not necessarily watching this conversation's SSE) stays
	// in sync — including the client lock holder so the "locked on X" banner
	// updates everywhere without a full reload.
	ModelSelected *utils.ModelSelected `json:"model_selected,omitempty"`
}

// WriteStatusEvent serializes a StatusEvent and writes it as SSE.
func WriteStatusEvent(w http.ResponseWriter, event StatusEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	// Same ErrAbortHandler protection as WriteSSEEvent — writing to a
	// disconnected global-status-stream client must never crash the process.
	defer func() {
		if r := recover(); r != nil {
			utils.Log("[SSE] status client connection aborted during write: %v", r)
		}
	}()

	restore := setWriteDeadline(w)
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	if err != nil {
		restore()
		return err
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	restore()
	return nil
}

// SetSSEHeaders configures the response writer for SSE streaming.
func SetSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Transfer-Encoding", "chunked")
}
