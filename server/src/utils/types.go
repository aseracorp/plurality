package utils

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// --- OpenAI-Compatible Message Types ---

// ContentImageURL holds the URL for an image content part.
type ContentImageURL struct {
	URL string `json:"url" bson:"url"`
}

// ContentPart represents one element of a multi-part content array.
// Used when a message contains mixed content (text + images).
type ContentPart struct {
	Type     string           `json:"type" bson:"type"` // "text" or "image_url"
	Text     string           `json:"text,omitempty" bson:"text,omitempty"`
	ImageURL *ContentImageURL `json:"image_url,omitempty" bson:"image_url,omitempty"`
}

// FunctionCall holds the function name and JSON-encoded arguments for a tool call.
type FunctionCall struct {
	Name      string `json:"name" bson:"name"`
	Arguments string `json:"arguments" bson:"arguments"`
}

// ToolCall represents an assistant's request to invoke a tool.
// Loading and IconURL are transient Plurality extensions — included in SSE
// events for live display but NOT persisted to DB.
type ToolCall struct {
	ID       string       `json:"id" bson:"id"`
	Type     string       `json:"type" bson:"type"` // "function"
	Function FunctionCall `json:"function" bson:"function"`
	Loading  string       `json:"loading,omitempty" bson:"-"` // transient: display template
	IconURL  string       `json:"icon_url,omitempty" bson:"-"` // transient: base64 icon
}

// Message is an OpenAI-compatible message.
//
// Content can be:
//   - string: simple text message
//   - []ContentPart: multi-part message (text + images)
//   - nil: for assistant messages that only have tool_calls
//
// Use TextContent() and ContentParts() for type-safe access.
type Message struct {
	Role       string      `json:"role" bson:"role"`                                     // "system", "user", "assistant", "tool"
	Content    interface{} `json:"content,omitempty" bson:"content,omitempty"`           // string or []ContentPart
	ToolCalls  []ToolCall  `json:"tool_calls,omitempty" bson:"tool_calls,omitempty"`     // assistant only
	ToolCallID string      `json:"tool_call_id,omitempty" bson:"tool_call_id,omitempty"` // tool role only
	Name       string      `json:"name,omitempty" bson:"name,omitempty"`                 // tool role only

	// Metadata — stored in DB, not sent to LLMs
	Timestamp   string `json:"timestamp,omitempty" bson:"timestamp,omitempty"`
	TotalTokens int    `json:"total_tokens,omitempty" bson:"total_tokens,omitempty"`
	Model       Model  `json:"model,omitempty" bson:"model,omitempty"`
}

// TextContent extracts the text from Content, regardless of whether it's a
// string or a []ContentPart. Returns empty string if no text is found.
func (m Message) TextContent() string {
	switch v := m.Content.(type) {
	case string:
		return v
	case []interface{}:
		for _, part := range v {
			if p, ok := part.(map[string]interface{}); ok {
				if p["type"] == "text" {
					if text, ok := p["text"].(string); ok {
						return text
					}
				}
			}
		}
	case []ContentPart:
		for _, part := range v {
			if part.Type == "text" {
				return part.Text
			}
		}
	}
	return ""
}

// ContentParts returns Content as []ContentPart. If Content is a plain string,
// it wraps it in a single text ContentPart. Returns nil if Content is nil.
func (m Message) ContentParts() []ContentPart {
	switch v := m.Content.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []ContentPart{{Type: "text", Text: v}}
	case []ContentPart:
		return v
	case []interface{}:
		var parts []ContentPart
		for _, raw := range v {
			if p, ok := raw.(map[string]interface{}); ok {
				part := ContentPart{}
				if t, ok := p["type"].(string); ok {
					part.Type = t
				}
				if t, ok := p["text"].(string); ok {
					part.Text = t
				}
				if imgURL, ok := p["image_url"].(map[string]interface{}); ok {
					if url, ok := imgURL["url"].(string); ok {
						part.ImageURL = &ContentImageURL{URL: url}
					}
				}
				parts = append(parts, part)
			}
		}
		return parts
	}
	return nil
}

// HasImages returns true if the message contains any image_url content parts.
func (m Message) HasImages() bool {
	for _, part := range m.ContentParts() {
		if part.Type == "image_url" {
			return true
		}
	}
	return false
}

// --- Conversation State Machine ---

type ConversationState string

const (
	StateIdle           ConversationState = "idle"
	StateProcessing     ConversationState = "processing"
	StateWaitingForTool ConversationState = "waiting_for_tool"
)

// --- Model & Configuration ---

type Model struct {
	Name   string            `json:"name,omitempty" bson:"name,omitempty"`
	Params map[string]string `json:"params,omitempty" bson:"params,omitempty"`
	Tools  []string          `json:"tools,omitempty" bson:"tools,omitempty"`
}

type ModelSelected struct {
	Text            *Model `json:"text,omitempty"`
	Vision          *Model `json:"vision,omitempty"`
	ImageGen        *Model `json:"image_gen,omitempty"`
	AudioTranscribe *Model `json:"audio_transcribe,omitempty"`
	VoiceGen        *Model `json:"voice_gen,omitempty"`
	AudioGen        *Model `json:"audio_gen,omitempty"`
	VideoGen        *Model `json:"video_gen,omitempty"`
	VideoVision     *Model `json:"video_vision,omitempty"`
	Code            *Model `json:"code,omitempty"`
}

// --- Conversation ---

type Conversation struct {
	ID            primitive.ObjectID `json:"id,omitempty" bson:"_id,omitempty"`
	UserID        string             `json:"user_id" bson:"user_id"`
	Messages      []Message          `json:"messages" bson:"messages"`
	Title         string             `json:"title" bson:"title"`
	LastMessageAt time.Time          `json:"last_message_at" bson:"last_message_at"`
	ModelSelected ModelSelected      `json:"model_selected" bson:"model_selected"`
	State         ConversationState  `json:"state" bson:"state"`
	MiniApp       *MiniApp           `json:"mini_app,omitempty" bson:"mini_app"`
	Folder        string             `json:"folder" bson:"folder"`
	Icon          string             `json:"icon" bson:"icon"`
}

// --- Billing ---

type UserAction struct {
	Type     int   `bson:"type"`
	Provider int   `bson:"provider"`
	Model    Model `bson:"model"`
}

// --- MiniApps ---

type MiniAppInput struct {
	Name        string   `json:"name" bson:"name"`
	Description string   `json:"description" bson:"description"`
	Type        string   `json:"type" bson:"type"`
	Options     []string `json:"options" bson:"options"`
}

type MiniApp struct {
	ID                      primitive.ObjectID `json:"id,omitempty" bson:"_id,omitempty"`
	Name                    string             `json:"name" bson:"name"`
	Description             string             `json:"description" bson:"description"`
	IconURL                 string             `json:"icon_url" bson:"icon_url"`
	Author                  string             `json:"author" bson:"author"`
	Prompt                  map[string]string  `json:"-" bson:"prompt"`
	ModelSelected           ModelSelected      `json:"model_selected" bson:"model_selected"`
	Inputs                  []MiniAppInput     `json:"inputs" bson:"inputs"`
	InitialMessage          map[string]string  `json:"initial_message" bson:"initial_message"`
	InputPlaceholderMessage map[string]string  `json:"input_placeholder_message" bson:"input_placeholder_message"`
	Form                    string             `json:"form" bson:"form"`
	Placeholder             string             `json:"placeholder" bson:"placeholder"`
}

// --- Tool Definitions (for LLM function calling) ---

type ToolsRequest struct {
	Type     string               `json:"type"`
	Function FunctionToolsRequest `json:"function"`
}

type FunctionToolsRequest struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  *ParameterToolsRequest `json:"parameters,omitempty"`
	InputSchema *ParameterToolsRequest `json:"input_schema,omitempty"` // Claude-specific
}

type ParameterToolsRequest struct {
	Type       string                                   `json:"type"`
	Properties map[string]PropertyParameterToolsRequest `json:"properties"`
	Required   []string                                 `json:"required,omitempty"`
}

type PropertyParameterToolsRequest struct {
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Enum        []string `json:"enum,omitempty"`
}

// AITool is a server-side tool registered in the tool registry.
type AITool struct {
	ID            primitive.ObjectID  `json:"id,omitempty" bson:"_id,omitempty"`
	Name          string              `json:"name" bson:"name"`
	Description   string              `json:"description" bson:"description"`
	ToolID        string              `json:"tool_id" bson:"tool_id"`
	ToolRequest   ToolsRequest        `json:"tool_call" bson:"tool_call"`
	LoadingString string              `json:"loading_string" bson:"loading_string"`
	IconURL       string              `json:"icon_url" bson:"icon_url"`
	Author        string              `json:"author" bson:"author"`
	Exec                    func(string) string          `json:"-"`
	Cost                    int                          `json:"cost" bson:"cost"`
	CostFunc                func(string) (float64, UserAction) `json:"-"`
}
