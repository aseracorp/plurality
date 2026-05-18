package ai

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/azukaar/plurality/src/utils"
)

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
}

// WriteSSEEvent serializes an SSEEvent and writes it to an HTTP response writer.
func WriteSSEEvent(w http.ResponseWriter, event SSEEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	if err != nil {
		return err
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
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
}

// WriteStatusEvent serializes a StatusEvent and writes it as SSE.
func WriteStatusEvent(w http.ResponseWriter, event StatusEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	if err != nil {
		return err
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// SetSSEHeaders configures the response writer for SSE streaming.
func SetSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Transfer-Encoding", "chunked")
}
