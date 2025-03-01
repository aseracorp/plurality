package ai

import (
	"go.mongodb.org/mongo-driver/bson/primitive"
	
	"github.com/azukaar/plurality/src/utils"
)


type ImageGenerationRequest struct {
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

type MessageReq struct {
	Role    string `json:"role"`
	Content []utils.MessageContent `json:"content"`
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
}

type ChatPayload struct {
	ConversationID primitive.ObjectID    `json:"conversation_id"`
	Messages []utils.Message `json:"messages"`
}