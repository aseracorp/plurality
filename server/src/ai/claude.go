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

	"github.com/azukaar/plurality/src/utils"
)

func SendChatCompletionClaude(model utils.Model, payload utils.Conversation) (io.ReadCloser, error) {
	var SystemPrompt = baseSystemPrompt +
		time.Now().String() +
		" on " +
		strconv.Itoa(time.Now().Day()) +
		"/" +
		strconv.Itoa(int(time.Now().Month())) +
		"/" +
		strconv.Itoa(time.Now().Year());

	apiKey := os.Getenv("CLAUDE_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("CLAUDE_API_KEY is not set")
	}

	utils.Debug("Payload: ", payload)

	// Claude API expects a different message format compared to OpenAI
	// Convert our messages to Claude's format
	msgList := payload.Messages
	claudeMessages := make([]ClaudeMessageReq, 0, len(msgList))
	
	for _, msg := range msgList {
		contents := make([]ClaudeContentReq, 0)
		
		for _, content := range msg.Content {
			if content.Type == "text" || content.Type == "snippet" {
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
	
	if modelName == "" {
		modelName = "claude-3-haiku-20240307" // Default model
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

	utils.Debug("A new chat request is being made with the following Claude model: %s", modelName)

	jsonData, err := json.Marshal(requestData)
	if err != nil {
		return nil, err
	}

	// Claude API endpoint
	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}

	// Set required headers for Claude API
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01") // Use appropriate version

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		strStatus := strconv.Itoa(resp.StatusCode)
		utils.Error("Claude API request failed with status", nil, strStatus, ":", string(respBody))
		return nil, fmt.Errorf("Claude API request failed with status %d", resp.StatusCode)
	}

	return resp.Body, nil
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
	// Usage    ClaudeUsage `json:"usage,omitempty"`
}

type ClaudeMessage struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Role     string `json:"role"`
	Content  []ClaudeContent `json:"content"`
	Model    string `json:"model"`
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
/*
// ProcessClaudeStream processes the streamed response from Claude API
// and converts it to the format expected by your application
func ProcessClaudeStream(responseBody io.ReadCloser, writer io.Writer) error {
	scanner := bufio.NewScanner(responseBody)
	defer responseBody.Close()
	
	for scanner.Scan() {
		line := scanner.Text()
		
		// Skip empty lines
		if line == "" {
			continue
		}
		
		// Remove "data: " prefix from SSE
		jsonData := strings.TrimPrefix(line, "data: ")
		
		// Handle the "[DONE]" marker
		if jsonData == "[DONE]" {
			break
		}
		
		// Parse the JSON chunk
		var claudeChunk ClaudeAIChunk
		if err := json.Unmarshal([]byte(jsonData), &claudeChunk); err != nil {
			utils.Error("Error parsing Claude response", err, "")
			continue
		}
		
		// Process based on chunk type
		switch claudeChunk.Type {
		case "message_start":
			// Message start doesn't typically contain content to stream
			continue
			
		case "content_block_start":
			// Content block start doesn't typically contain text to stream
			continue
			
		case "content_block_delta":
			// This is the main streaming text from Claude
			if claudeChunk.Delta.Type == "text" && claudeChunk.Delta.Text != "" {
				// Convert Claude delta to your app's expected format
				aiChunk := AIChunk{
					Model: claudeChunk.Message.Model,
					Choices: []struct {
						Text  string `json:"text"`
						Delta struct {
							Content string `json:"content"`
						} `json:"delta"`
					}{
						{
							Delta: struct {
								Content string `json:"content"`
							}{
								Content: claudeChunk.Delta.Text,
							},
						},
					},
				}
				
				// Marshal to JSON and write to the output
				chunkBytes, err := json.Marshal(aiChunk)
				if err != nil {
					utils.Error("Error marshaling chunk", err, "")
					continue
				}
				
				if _, err := writer.Write([]byte("data: " + string(chunkBytes) + "\n\n")); err != nil {
					utils.Error("Error writing response", err, "")
					return err
				}
			}
			
		case "content_block_stop":
			// Content block stop doesn't typically contain text to stream
			continue
			
		case "message_delta":
			// Message deltas might not contain text to stream
			continue
			
		case "message_stop":
			// Message stop signals the end of the message
			break
			
		}
	}
	
	// Write the [DONE] marker at the end
	if _, err := writer.Write([]byte("data: [DONE]\n\n")); err != nil {
		utils.Error("Error writing final response", err, "")
		return err
	}
	
	return scanner.Err()
}*/