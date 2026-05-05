package ai

import (
	"github.com/azukaar/plurality/src/utils"
)

// --- Image Generation ---

type ImageGenerationRequest struct {
	ConversationID string `json:"conversation_id"`
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
	ConversationID  string                       `json:"conversation_id"`
	ModelSelected   utils.ModelSelected          `json:"model_selected"`
	Messages        []utils.Message              `json:"messages"`
	ToolResults     []utils.Message              `json:"tool_results"`
	MiniApp         utils.MiniApp                `json:"mini_app"`
	IsCall          bool                         `json:"is_call"`
	ClientSideTools []utils.FunctionToolsRequest `json:"client_side_tools"`
	AvailableSkills []string                     `json:"available_skills,omitempty"`
	// HasAttachedFolder signals that the user has attached a local folder to
	// the conversation. The server force-includes the device-side filesystem
	// tool schemas when true, so the LLM can act on the attached folder.
	HasAttachedFolder bool `json:"has_attached_folder,omitempty"`
}

// --- Provider Request Types ---

// StreamOptions enables usage information in streaming responses.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// StandardContentReq is a content block in an OpenAI request message.
type StandardContentReq struct {
	Type     string                 `json:"type"`
	Text     string                 `json:"text,omitempty"`
	ImageURL *utils.ContentImageURL `json:"image_url,omitempty"`
}

// StandardMessageReq is a message in an OpenAI request.
type StandardMessageReq struct {
	Role       string           `json:"role"`
	Content    interface{}      `json:"content"`                // string or []StandardContentReq
	ToolCalls  []utils.ToolCall `json:"tool_calls,omitempty"`   // assistant messages with tool calls
	ToolCallID string           `json:"tool_call_id,omitempty"` // tool messages
	Name       string           `json:"name,omitempty"`         // tool messages
}

// StandardChatRequest is the request body sent to the LiteLLM proxy (OpenAI-compatible).
type StandardChatRequest struct {
	Model             string               `json:"model"`
	Messages          []StandardMessageReq `json:"messages"`
	MaxTokens         *int                 `json:"max_tokens,omitempty"`
	MaxCompletionToks *int                 `json:"max_completion_tokens,omitempty"`
	Temperature       *float64             `json:"temperature,omitempty"`
	TopP              float64              `json:"top_p,omitempty"`
	TopK              int                  `json:"top_k,omitempty"`
	RepetitionPenalty float64              `json:"repetition_penalty,omitempty"`
	Stop              []string             `json:"stop,omitempty"`
	Stream            bool                 `json:"stream"`
	StreamOptions     *StreamOptions       `json:"stream_options,omitempty"`
	Tools             []utils.ToolsRequest `json:"tools,omitempty"`
}

// --- Standard streaming chunk (OpenAI format, used for all providers via LiteLLM) ---

type AIChunk struct {
	Model string `json:"model"`
	Usage struct {
		TotalTokens      int     `json:"total_tokens"`
		CompletionTokens int     `json:"completion_tokens"`
		PromptTokens     int     `json:"prompt_tokens"`
		ResponseCost     float64 `json:"response_cost,omitempty"`
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
