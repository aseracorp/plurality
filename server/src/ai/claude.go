package ai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	// "bufio"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"context"

	"github.com/azukaar/plurality/src/utils"
	"github.com/azukaar/plurality/src/db"
)

func SendChatCompletionClaude(ctx context.Context, model utils.Model, payload utils.Conversation, systemPrompt string) (io.ReadCloser, int, error) {
	var SystemPrompt = systemPrompt +
		time.Now().String() +
		" on " +
		strconv.Itoa(time.Now().Day()) +
		"/" +
		strconv.Itoa(int(time.Now().Month())) +
		"/" +
		strconv.Itoa(time.Now().Year());

	apiKey := os.Getenv("CLAUDE_API_KEY")
	if apiKey == "" {
		return nil, 0, fmt.Errorf("CLAUDE_API_KEY is not set")
	}

	utils.Debug("Payload: ", payload)
	
	if model.Name == "" {
		model.Name = "Claude/claude-3-haiku-20240307" // Default model
	}


	err, priceToken := GetPrice(TEXT_INPUT, CLAUDE, model, SystemPrompt)
	if err != nil {
		return nil, 0, err
	}

	// Claude API expects a different message format compared to OpenAI
	// Convert our messages to Claude's format
	msgList := payload.Messages
	claudeMessages := make([]ClaudeMessageReq, 0, len(msgList))
	
	for _, msg := range msgList {
		contents := make([]ClaudeContentReq, 0)
		
		for _, content := range msg.Content {
			if content.Type == "text" || content.Type == "snippet" {
				err, _priceToken := GetPrice(TEXT_INPUT, CLAUDE, model, content.Text + " {}{}{}{}{}{}{}")
				priceToken += _priceToken

				if err != nil {
					return nil, 0, err
				}

				contents = append(contents, ClaudeContentReq{
					Type: "text",
					Text: content.Text,
				})
			} else if content.Type == "image_url" {
				continue
				// Handle image content if provided
				// Claude API requires images to be base64 encoded
				// This is a simplified version - actual implementation might need adjustments
				// contents = append(contents, ClaudeContentReq{
				// 	Type: "image",
				// 	Source: ClaudeImageSourceReq{
				// 		Type: "base64",
				// 		Media_type: "image/jpeg", // Adjust based on actual image type
				// 		Data: content.ImageURL.URL, // Assuming this contains base64 data
				// 	},
				// })
			}
		}
		
		claudeMessages = append(claudeMessages, ClaudeMessageReq{
			Role:    convertRoleToClaude(msg.Role),
			Content: contents,
		})
	}

	// Extract Claude model version from prefix if it exists
	modelName := model.Name
	if strings.HasPrefix(model.Name, "Claude/") {
		modelName = strings.TrimPrefix(model.Name, "Claude/")
	}
	
	if modelName == "claude-3-haiku" {
		modelName = "claude-3-haiku-20240307"
	}

	if modelName == "claude-3-7-sonnet" {
		modelName = "claude-3-7-sonnet-20250219"
	}
	
	if model.Params == nil {
		model.Params = make(map[string]string)
	}

	temperature := 0.7
	if model.Params["temperature"] != "" {
		temp, err := strconv.ParseFloat(model.Params["temperature"], 64)
		if err == nil {
			temperature = temp
		}
	}

	maxTokens := 4096
	if model.Params["max_tokens"] != "" {
		tokens, err := strconv.Atoi(model.Params["max_tokens"])
		if err == nil {
			maxTokens = tokens
		}
	}

	// Claude API request structure
	requestData := ClaudeChatRequest{
		Model:       modelName,
		Messages:    claudeMessages,
		MaxTokens:   maxTokens,
		Temperature: temperature,
		Stream:      true,
		System:      SystemPrompt,
	}

	// Check balance
	canPerform, err := db.CheckSufficientCredits(ctx, priceToken + 1.0)
	if err != nil {	
		return nil, 0, err
	}

	if !canPerform {
		return nil, 0, fmt.Errorf("insufficient credits for this action")
	}

	utils.Debug("A new chat request is being made with the following Claude model: %s", modelName)

	jsonData, err := json.Marshal(requestData)
	if err != nil {
		return nil, 0, err
	}

	// Claude API endpoint
	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, 0, err
	}

	// Set required headers for Claude API
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01") // Use appropriate version

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}

	// deduce the price of the request
	// _, err = db.RemoveCredits(ctx, priceToken, utils.UserAction{
	// 	Type: TEXT_INPUT,
	// 	Provider: CLAUDE,
	// 	Model: model,
	// })

	if err != nil {
		utils.Error("Error removing credits: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		strStatus := strconv.Itoa(resp.StatusCode)
		utils.Error("Claude API request failed with status", nil, strStatus, ":", string(respBody))
		return nil, 0, fmt.Errorf("Claude API request failed with status %d", resp.StatusCode)
	}

	return resp.Body, int(priceToken), nil
}

// Helper function to convert standard role to Claude-specific role
func convertRoleToClaude(role string) string {
	switch role {
	case "user":
		return "user"
	case "assistant":
		return "assistant"
	case "system":
		return "system"
	default:
		return "user" // Default to user for unknown roles
	}
}

// Claude-specific request structures
type ClaudeContentReq struct {
	Type   string              `json:"type"`
	Text   string              `json:"text,omitempty"`
	// Source ClaudeImageSourceReq `json:"source,omitempty"`
}

type ClaudeImageSourceReq struct {
	Type       string `json:"type"`
	Media_type string `json:"media_type"`
	Data       string `json:"data"`
}

type ClaudeMessageReq struct {
	Role    string             `json:"role"`
	Content []ClaudeContentReq `json:"content"`
}

type ClaudeChatRequest struct {
	Model       string             `json:"model"`
	Messages    []ClaudeMessageReq `json:"messages"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature float64            `json:"temperature"`
	Stream      bool               `json:"stream"`
	System      string             `json:"system"`
}

// ClaudeAIChunk represents a chunk of the streaming response from Claude API
type ClaudeAIChunk struct {
	Type     string `json:"type"`
	Delta    ClaudeDelta `json:"delta,omitempty"`
	Usage    ClaudeUsage `json:"usage,omitempty"`
	Message  ClaudeMessage `json:"message,omitempty"`
}

type ClaudeMessage struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Role     string `json:"role"`
	Content  []ClaudeContent `json:"content"`
	Model    string `json:"model"`
	Usage 	 ClaudeUsage `json:"usage"`
}

type ClaudeDelta struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
}

type ClaudeContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type ClaudeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}
