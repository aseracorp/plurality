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
	"github.com/azukaar/plurality/src/ai_tools"
)


func SendChatCompletionClaude(ctx context.Context, model utils.Model, conv utils.Conversation, systemPrompt string, payload ChatPayload) (io.ReadCloser, int, error) {
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

	utils.Debug("conv: ", conv)
	
	if model.Name == "" {
		model.Name = "Claude/claude-3-haiku-20240307" // Default model
	}


	err, priceToken := GetPrice(TEXT_INPUT, CLAUDE, model, SystemPrompt)
	if err != nil {
		return nil, 0, err
	}

	// Claude API expects a different message format compared to OpenAI
	// Convert our messages to Claude's format
	msgList := conv.Messages
	claudeMessages := make([]ClaudeMessageReq, 0, len(msgList))
	
	for _, msg := range msgList {
		contents := make([]MessageContentReq, 0)
		
		for _, content := range msg.Content {
			if content.Type == "tool_use" {
				if content.ToolCall.Arguments == "" {
					content.ToolCall.Arguments = "{\"_iamhere\": \"true\"}"
				}
				args := utils.ParseJsonString(content.ToolCall.Arguments)
				contents = append(contents, MessageContentReq{
					Type: content.Type,
					ID: content.ToolCall.ID,
					Name: content.ToolCall.Name,
					Input: args,
				})
			}

			if content.Type == "snippet" {
				content.Type = "text"
			}

			if content.Type == "tool_result" {
				if content.Text !="" {
					err, _priceToken := GetPrice(TEXT_INPUT, CLAUDE, model, content.Text + " {}{}{}{}{}{}{}")
					priceToken += _priceToken

					if err != nil {
						return nil, 0, err
					}

					contents = append(contents, MessageContentReq{
						Type: content.Type,
						ToolUseId: content.ToolUseId,
						Content: content.Text,
					})
				}
			}
			
			if content.Type == "text" {
				if content.Text !="" {
					err, _priceToken := GetPrice(TEXT_INPUT, CLAUDE, model, content.Text + " {}{}{}{}{}{}{}")
					priceToken += _priceToken

					if err != nil {
						return nil, 0, err
					}

					contents = append(contents, MessageContentReq{
						Type: content.Type,
						Text: content.Text,
						ToolUseId: content.ToolUseId,
					})
				}
			} else if content.Type == "image_url" {
				if msg.Role == "user" {
					curl := content.ImageURL.URL
					curl = strings.TrimPrefix(curl, "data:image/jpeg;base64,")
					// Handle image content if provided
					// Claude API requires images to be base64 encoded
					// This is a simplified version - actual implementation might need adjustments
					contents = append(contents, MessageContentReq{
						Type: "image",
						Source: &ClaudeImageSourceReq{
							Type: "base64",
							Media_type: "image/jpeg", // Adjust based on actual image type
							Data: curl, // Assuming this contains base64 data
						},
					})
				} else {
					continue
				}
			}
		}
		
		if len(contents) > 0 {
			claudeMessages = append(claudeMessages, ClaudeMessageReq{
				Role:    msg.Role,
				Content: contents,
			})
		}
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
	
	aitools := ai_tools.GetClaudeRequests(model, payload.ClientSideTools)
	if CheckActionModel(model.Name) && len(aitools) > 0 {
		requestData.Tools = aitools
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

	utils.Debug("Request data: ", string(jsonData))

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

// Claude-specific request structures

type ClaudeMessageReq struct {
	Role    string             `json:"role"`
	Content []MessageContentReq `json:"content"`
}

type ClaudeChatRequest struct {
	Model       string             `json:"model"`
	Messages    []ClaudeMessageReq `json:"messages"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature float64            `json:"temperature"`
	Stream      bool               `json:"stream"`
	System      string             `json:"system"`
	Tools       []utils.FunctionToolsRequest `json:"tools,omitempty"`
}

// ClaudeAIChunk represents a chunk of the streaming response from Claude API
type ClaudeAIChunk struct {
	Type     string `json:"type"`
	Delta    ClaudeDelta `json:"delta,omitempty"`
	Usage    ClaudeUsage `json:"usage,omitempty"`
	Message  ClaudeMessage `json:"message,omitempty"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block,omitempty"`
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
