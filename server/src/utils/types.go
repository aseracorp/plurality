package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// --- OpenAI-Compatible Message Types ---

// ContentImageURL holds the URL for an image content part.
type ContentImageURL struct {
	URL string `json:"url"`
}

// ContentPart represents one element of a multi-part content array.
// Used when a message contains mixed content (text + images).
type ContentPart struct {
	Type     string           `json:"type"`                       // "text", "image_url", "snippet", "file", "pdf", "docx", "xlsx", "pptx"
	Text     string           `json:"text,omitempty"`
	ImageURL *ContentImageURL `json:"image_url,omitempty"`
	Filename string           `json:"filename,omitempty"`
}

// --- MessageContent ---

// MessageContent holds message content as a normalized []ContentPart slice.
// It implements custom JSON marshaling so that:
//   - A single text-only part is serialized as a plain string (compact, API-compatible)
//   - Multiple parts are serialized as an array of content-part objects
type MessageContent struct {
	parts []ContentPart
}

// NewTextContent creates a MessageContent from a plain string.
func NewTextContent(s string) MessageContent {
	if s == "" {
		return MessageContent{}
	}
	return MessageContent{parts: []ContentPart{{Type: "text", Text: s}}}
}

// NewPartsContent creates a MessageContent from a slice of ContentPart.
func NewPartsContent(parts []ContentPart) MessageContent {
	return MessageContent{parts: parts}
}

// TextContent returns the text of the first "text" content part, or "".
func (mc MessageContent) TextContent() string {
	for _, part := range mc.parts {
		if part.Type == "text" {
			return part.Text
		}
	}
	return ""
}

// ContentParts returns the underlying []ContentPart slice.
func (mc MessageContent) ContentParts() []ContentPart {
	return mc.parts
}

// HasImages returns true if any part is an image_url.
func (mc MessageContent) HasImages() bool {
	for _, part := range mc.parts {
		if part.Type == "image_url" {
			return true
		}
	}
	return false
}

// IsZero implements bsoncodec.Zeroer so BSON omitempty works.
func (mc MessageContent) IsZero() bool {
	return len(mc.parts) == 0
}

// --- JSON marshaling ---

func (mc MessageContent) MarshalJSON() ([]byte, error) {
	if len(mc.parts) == 0 {
		return []byte("null"), nil
	}
	if len(mc.parts) == 1 && mc.parts[0].Type == "text" && mc.parts[0].ImageURL == nil {
		return json.Marshal(mc.parts[0].Text)
	}
	return json.Marshal(mc.parts)
}

func (mc *MessageContent) UnmarshalJSON(data []byte) error {
	if string(data) == "null" || len(data) == 0 {
		mc.parts = nil
		return nil
	}
	// Try string first (most common)
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		if s != "" {
			mc.parts = []ContentPart{{Type: "text", Text: s}}
		} else {
			mc.parts = nil
		}
		return nil
	}
	// Try array of ContentPart
	var parts []ContentPart
	if err := json.Unmarshal(data, &parts); err != nil {
		return fmt.Errorf("MessageContent: expected string or array: %w", err)
	}
	mc.parts = parts
	return nil
}

// FunctionCall holds the function name and JSON-encoded arguments for a tool call.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolCall represents an assistant's request to invoke a tool.
// Loading and IconURL are transient Plurality extensions — included in SSE
// events for live display but NOT persisted to DB.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`                    // "function"
	Function FunctionCall `json:"function"`
	Loading  string       `json:"loading,omitempty"`        // transient: display template
	IconURL  string       `json:"icon_url,omitempty"`       // transient: base64 icon
}

// Message is an OpenAI-compatible message.
//
// Content is a MessageContent that is internally always []ContentPart.
// Use TextContent() and ContentParts() for access.
type Message struct {
	Role       string         `json:"role"`                           // "system", "user", "assistant", "tool"
	Content    MessageContent `json:"content,omitempty"`
	ToolCalls  []ToolCall     `json:"tool_calls,omitempty"`           // assistant only
	ToolCallID string         `json:"tool_call_id,omitempty"`         // tool role only
	Name       string         `json:"name,omitempty"`                 // tool role only

	// Metadata — stored in DB, not sent to LLMs
	Timestamp   string `json:"timestamp,omitempty"`
	TotalTokens int    `json:"total_tokens,omitempty"`
	Model       Model  `json:"model,omitempty"`
}

// TextContent returns the text of the first "text" content part.
func (m Message) TextContent() string { return m.Content.TextContent() }

// ContentParts returns the content as []ContentPart.
func (m Message) ContentParts() []ContentPart { return m.Content.ContentParts() }

// HasImages returns true if the message contains any image_url content parts.
func (m Message) HasImages() bool { return m.Content.HasImages() }

// --- Conversation State Machine ---

type ConversationState string

const (
	StateIdle               ConversationState = "idle"
	StateProcessing         ConversationState = "processing"
	StateWaitingForTool     ConversationState = "waiting_for_tool"
	StateWaitingForApproval ConversationState = "waiting_for_approval"
)

// --- Model & Configuration ---

type Model struct {
	Name   string            `json:"name,omitempty" bson:"name,omitempty"`
	Params map[string]string `json:"params,omitempty" bson:"params,omitempty"`
	Tools  map[string]string `json:"tools,omitempty" bson:"tools,omitempty"`
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
	ID            string            `json:"id,omitempty"`
	UserID        string            `json:"user_id"`
	Messages      []Message         `json:"messages"`
	Title         string            `json:"title"`
	LastMessageAt time.Time         `json:"last_message_at"`
	ModelSelected ModelSelected     `json:"model_selected"`
	State         ConversationState `json:"state"`
	MiniApp       *MiniApp          `json:"mini_app,omitempty"`
	Folder        string            `json:"folder"`
	Icon          string            `json:"icon"`
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
	Exec func(context.Context, string, Conversation) MessageContent `json:"-"`
	Cost int                                       `json:"cost" bson:"cost"`
	CostFunc                func(string) (float64, UserAction) `json:"-"`
}
