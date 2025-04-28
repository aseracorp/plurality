package ai

import (
	"go.mongodb.org/mongo-driver/bson/primitive"
	
	"github.com/azukaar/plurality/src/utils"
)

type ImageGenerationRequest struct {
	ConversationID primitive.ObjectID `json:"conversation_id"`
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	Steps          int    `json:"steps"`
	N              int    `json:"n"`
	ResponseFormat string `json:"response_format"`
}

type ImageGenerationResponse struct {
	B64JSON string `json:"b64_json"`
}

type ClaudeImageSourceReq struct {
	Type       string `json:"type"`
	Media_type string `json:"media_type"`
	Data       string `json:"data"`
}

type MessageContentReq struct {
	Type   string              `json:"type"`
	Text   string              `json:"text,omitempty"`
	ToolUseId string `json:"tool_use_id,omitempty"`
	Content string `json:"content,omitempty"`
	ImageURL *utils.MessageContentURL `json:"image_url,omitempty"`
	
	Source *ClaudeImageSourceReq `json:"source,omitempty"`
	
	// for tool_use
	ID string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	Input map[string]string `json:"input,omitempty"`
}

type MessageReq struct {
	Role    string `json:"role"`
	Content []MessageContentReq `json:"content"`
}

type ChatRequest struct {
	Model             string    `json:"model"`
	Messages          []MessageReq `json:"messages"`
	MaxTokens         *int      `json:"max_tokens"`
	Temperature       float64   `json:"temperature"`
	TopP              float64   `json:"top_p"`
	TopK              int       `json:"top_k"`
	RepetitionPenalty float64   `json:"repetition_penalty"`
	Stop              []string  `json:"stop"`
	Stream            bool      `json:"stream"`
	Tools 					  []utils.ToolsRequest  `json:"tools,omitempty"`
}

type ChatRequestChatGPT struct {
	Model             string       `json:"model"`
	Messages          []MessageReq `json:"messages"`
	MaxTokens         *int         `json:"max_completion_tokens"`
	Temperature       *float64      `json:"temperature,omitempty"`
	TopP              *float64      `json:"top_p,omitempty"`
	Stream						bool         `json:"stream"`	
	Tools 					  []utils.ToolsRequest  `json:"tools,omitempty"`
}

type ChatPayload struct {
	ConversationID  primitive.ObjectID    `json:"conversation_id"`
	ModelSelected		utils.ModelSelected `json:"model_selected"`
	Messages 				[]utils.Message `json:"messages"`
	MiniApp         utils.MiniApp         `json:"mini_app"`
	IsCall 					bool                `json:"is_call"`
	ClientSideTools []utils.FunctionToolsRequest `json:"client_side_tools"`
}

type AIChunk struct {
	Model string `json:"model"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		PromptTokens int `json:"prompt_tokens"`
	} `json:"usage,omitempty"`
	Choices []struct {
		Text  string `json:"text"`
		Delta struct {
			Content string `json:"content"`
			ToolCalls []utils.ToolCall `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
}


// Command represents a parsed command with its type and arguments
type Command struct {
	Type string
	Args string
}
