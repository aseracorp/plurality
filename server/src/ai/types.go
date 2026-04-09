package ai

import (
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/azukaar/plurality/src/utils"
)

// --- Image Generation ---

type ImageGenerationRequest struct {
	ConversationID primitive.ObjectID `json:"conversation_id"`
	Model          string             `json:"model"`
	Prompt         string             `json:"prompt"`
	Width          int                `json:"width"`
	Height         int                `json:"height"`
	Steps          int                `json:"steps"`
	N              int                `json:"n"`
	ResponseFormat string             `json:"response_format"`
}

type ImageGenerationResponse struct {
	B64JSON string `json:"b64_json"`
}

// --- Chat API Payload ---

// ChatPayload is the request body for POST /chat.
// Either Messages or ToolResults should be non-empty, not both.
type ChatPayload struct {
	ConversationID  primitive.ObjectID           `json:"conversation_id"`
	ModelSelected   utils.ModelSelected          `json:"model_selected"`
	Messages        []utils.Message              `json:"messages"`
	ToolResults     []utils.Message              `json:"tool_results"`
	MiniApp         utils.MiniApp                `json:"mini_app"`
	IsCall          bool                         `json:"is_call"`
	ClientSideTools []utils.FunctionToolsRequest `json:"client_side_tools"`
}

// --- Provider-Specific Request Types ---

// These are internal types for building requests to each LLM provider.
// They are NOT used in the client-server API or DB.

// StandardContentReq is a content block in an OpenAI/Fireworks request message.
type StandardContentReq struct {
	Type     string                 `json:"type"`
	Text     string                 `json:"text,omitempty"`
	ImageURL *utils.ContentImageURL `json:"image_url,omitempty"`
}

// StandardMessageReq is a message in an OpenAI/Fireworks request.
type StandardMessageReq struct {
	Role       string           `json:"role"`
	Content    interface{}      `json:"content"`                // string or []StandardContentReq
	ToolCalls  []utils.ToolCall `json:"tool_calls,omitempty"`   // assistant messages with tool calls
	ToolCallID string           `json:"tool_call_id,omitempty"` // tool messages
	Name       string           `json:"name,omitempty"`         // tool messages
}

// StandardChatRequest is the request body for OpenAI/Fireworks APIs.
type StandardChatRequest struct {
	Model             string               `json:"model"`
	Messages          []StandardMessageReq `json:"messages"`
	MaxTokens         *int                 `json:"max_tokens,omitempty"`
	MaxCompletionToks *int                 `json:"max_completion_tokens,omitempty"` // OpenAI-specific
	Temperature       *float64             `json:"temperature,omitempty"`
	TopP              float64              `json:"top_p,omitempty"`
	TopK              int                  `json:"top_k,omitempty"`
	RepetitionPenalty float64              `json:"repetition_penalty,omitempty"`
	Stop              []string             `json:"stop,omitempty"`
	Stream            bool                 `json:"stream"`
	Tools             []utils.ToolsRequest `json:"tools,omitempty"`
}

// Claude-specific types

type ClaudeImageSourceReq struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type ClaudeContentReq struct {
	Type      string                `json:"type"`
	Text      string                `json:"text,omitempty"`
	ToolUseID string                `json:"tool_use_id,omitempty"`
	Content   string                `json:"content,omitempty"` // tool_result content
	ID        string                `json:"id,omitempty"`      // tool_use ID
	Name      string                `json:"name,omitempty"`    // tool_use name
	Input     map[string]string     `json:"input,omitempty"`   // tool_use arguments
	Source    *ClaudeImageSourceReq `json:"source,omitempty"`
}

type ClaudeMessageReq struct {
	Role    string             `json:"role"`
	Content []ClaudeContentReq `json:"content"`
}

type ClaudeChatRequest struct {
	Model       string                       `json:"model"`
	Messages    []ClaudeMessageReq           `json:"messages"`
	MaxTokens   int                          `json:"max_tokens"`
	Temperature float64                      `json:"temperature"`
	Stream      bool                         `json:"stream"`
	System      string                       `json:"system"`
	Tools       []utils.FunctionToolsRequest `json:"tools,omitempty"`
}

type ClaudeAIChunk struct {
	Type         string        `json:"type"`
	Delta        ClaudeDelta   `json:"delta,omitempty"`
	Usage        ClaudeUsage   `json:"usage,omitempty"`
	Message      ClaudeMessage `json:"message,omitempty"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block,omitempty"`
}

type ClaudeMessage struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content []ClaudeContent `json:"content"`
	Model   string          `json:"model"`
	Usage   ClaudeUsage     `json:"usage"`
}

type ClaudeDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	PartialJson string `json:"partial_json,omitempty"`
}

type ClaudeContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type ClaudeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// --- Standard streaming chunk (OpenAI/Fireworks) ---

type AIChunk struct {
	Model string `json:"model"`
	Usage struct {
		TotalTokens      int `json:"total_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		PromptTokens     int `json:"prompt_tokens"`
	} `json:"usage,omitempty"`
	Choices []struct {
		Text  string `json:"text"`
		Delta struct {
			Content   string           `json:"content"`
			ToolCalls []utils.ToolCall `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
}

// Command represents a parsed command with its type and arguments
type Command struct {
	Type string
	Args string
}
