package ai

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/azukaar/plurality/src/utils"
	"github.com/azukaar/plurality/src/db"
)

// ChunkProcessor encapsulates the context and methods for processing chunks
type ChunkProcessor struct {
	W              http.ResponseWriter
	Conv           utils.Conversation
	IsNew          bool
	TokenUsage     int
	StringProduced string
	ModelName      string
}

// NewChunkProcessor creates a new ChunkProcessor instance
func NewChunkProcessor(w http.ResponseWriter, conv utils.Conversation, isNew bool) *ChunkProcessor {
	return &ChunkProcessor{
		W:              w,
		Conv:           conv,
		IsNew:          isNew,
		TokenUsage:     0,
		StringProduced: "",
	}
}

// ProcessStandardChunk processes SSE chunks for standard API responses (Together AI and OpenAI)
func (cp *ChunkProcessor) ProcessStandardChunk(ctx context.Context, response io.ReadCloser, modelSelected utils.Model) error {
	defer response.Close()

	// Process the SSE chunks
	scanner := bufio.NewScanner(response)
	for scanner.Scan() {
		line := scanner.Text()

		// Skip empty lines or non-data lines
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		// Extract the JSON payload
		jsonData := strings.TrimPrefix(line, "data: ")

		utils.Debug("[HandleChat] Received chunk %s", jsonData)

		// Check for the [DONE] marker
		if jsonData == "[DONE]" {
			if cp.TokenUsage == 0 {
				err, tu := GetTokenNumber(modelSelected, cp.StringProduced)
				if err != nil {
					utils.MajorError("[HandleChat] Error getting token number", err)
				}
				cp.TokenUsage = int(tu)
			}

			if cp.IsNew {
				//cp.handleNewConversationTitle(ctx)
			}

			fmt.Fprintf(cp.W, "%s\n\n", line)
			cp.W.(http.Flusher).Flush()

			// Push message to DB
			cp.saveMessageToDB(ctx)

			provider := TOGETHER
			if strings.HasPrefix(cp.ModelName, "ChatGPT/") {
				provider = OPENAI
			}

			priceToken := GetPriceFromTokenUsage(TEXT_OUTPUT, provider, modelSelected, float64(cp.TokenUsage))
			_, err := db.RemoveCredits(ctx, priceToken, utils.UserAction{
				Type: TEXT_OUTPUT,
				Provider: provider,
				Model: modelSelected,
			})
			if err != nil {
				utils.MajorError("[HandleChat] Error removing credits", err)
			}
			
			utils.Log("[HandleChat] Successfully completed chat using %d tokens", cp.TokenUsage)
			break
		}

		// Parse the JSON
		var chunk AIChunk
		if err := json.Unmarshal([]byte(jsonData), &chunk); err != nil {
			utils.Error("[HandleChat] Error parsing chunk", err, jsonData)
			continue
		}

		if chunk.Usage.CompletionTokens > 0 {
			utils.Debug("[HandleChat] Completion tokens is : %d", chunk.Usage.PromptTokens)
			cp.TokenUsage = chunk.Usage.CompletionTokens
		}
		if chunk.Usage.PromptTokens > 0 {
			// Only for TogetherAI
			utils.Debug("[HandleChat] Prompt Tokens is : %d", chunk.Usage.PromptTokens)
			priceToken := GetPriceFromTokenUsage(TEXT_INPUT, TOGETHER, modelSelected, float64(chunk.Usage.PromptTokens))
			_, err := db.RemoveCredits(ctx, priceToken, utils.UserAction{
				Type: TEXT_INPUT,
				Provider: TOGETHER,
				Model: modelSelected,
			})
			if err != nil {
				utils.MajorError("[HandleChat] Error removing credits", err)
			}
		}

		cp.ModelName = chunk.Model

		// Extract content from the chunk
		if len(chunk.Choices) > 0 {
			var content string

			// Try to get content from delta first (streaming format)
			if chunk.Choices[0].Delta.Content != "" {
				content = chunk.Choices[0].Delta.Content
			} else if chunk.Choices[0].Text != "" {
				// Fall back to text if available
				content = chunk.Choices[0].Text
			}

			// Only process non-empty content
			if content != "" {
				cp.StringProduced += content

				cp.sendChunkToClient(content)
			}
		}
	}

	return scanner.Err()
}

// ProcessClaudeChunk processes SSE chunks specifically for Claude API responses
func (cp *ChunkProcessor) ProcessClaudeChunk(ctx context.Context, response io.ReadCloser, modelSelected utils.Model) error {
	defer response.Close()

	scanner := bufio.NewScanner(response)
	for scanner.Scan() {
		line := scanner.Text()

		utils.Debug("[HandleChat] Received Claude chunk %s", line)

		// Skip empty lines
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}

		// Remove "data: " prefix from SSE
		jsonData := strings.TrimPrefix(line, "data: ")

		utils.Debug("[HandleChat] Processed Claude chunk %s", jsonData)

		// Parse the JSON chunk
		var claudeChunk ClaudeAIChunk
		if err := json.Unmarshal([]byte(jsonData), &claudeChunk); err != nil {
			utils.Error("[HandleChat] Error parsing Claude response", err, "")
			continue
		}

		// // Update model name if available
		// if claudeChunk.Message.Model != "" {
		// 	cp.ModelName = claudeChunk.Message.Model
		// }

		// // Update token usage if available
		if claudeChunk.Usage.OutputTokens > 0 {
			cp.TokenUsage = claudeChunk.Usage.OutputTokens
		}
		
		utils.Debug("[HandleChat] caca: %d", claudeChunk.Message.Usage.InputTokens)
		if claudeChunk.Message.Usage.InputTokens > 0 {
			utils.Log("[HandleChat] Input tokens: %d", claudeChunk.Message.Usage.InputTokens)

			priceToken := GetPriceFromTokenUsage(TEXT_INPUT, CLAUDE, modelSelected, float64(claudeChunk.Message.Usage.InputTokens))
			_, err := db.RemoveCredits(ctx, priceToken, utils.UserAction{
				Type: TEXT_INPUT,
				Provider: CLAUDE,
				Model: modelSelected,
			})

			if err != nil {
				utils.Error("[HandleChat] Error removing credits", err);
			}
		}	

		// Process based on chunk type
		switch claudeChunk.Type {
		case "message_stop":
			if cp.IsNew {
				//cp.handleNewConversationTitle(ctx)
			}

			fmt.Fprintf(cp.W, "[DONE]\n", line)
			cp.W.(http.Flusher).Flush()

			// Push message to DB
			cp.saveMessageToDB(ctx)

			priceToken := GetPriceFromTokenUsage(TEXT_OUTPUT, CLAUDE, modelSelected, float64(cp.TokenUsage))

			db.RemoveCredits(ctx, priceToken, utils.UserAction{
				Type: TEXT_OUTPUT,
				Provider: CLAUDE,
				Model: modelSelected,
			})
			
			utils.Log("[HandleChat] Successfully completed Claude chat using %d tokens", cp.TokenUsage)
			break
		case "content_block_delta":
			// This is the main streaming text from Claude
			if claudeChunk.Delta.Type == "text_delta" && claudeChunk.Delta.Text != "" {
				content := claudeChunk.Delta.Text
				cp.StringProduced += content
				cp.sendChunkToClient(content)
			}
		}
	}

	return scanner.Err()
}

// sendChunkToClient formats and sends a chunk of text to the client
func (cp *ChunkProcessor) sendChunkToClient(content string) {
	if os.Getenv("LOG_LEVEL") == "DEBUG" {
		// utils.Debug("[HandleChat] Sending response", content)
	}

	responseObj := map[string]interface{}{
		"content":          content,
		"model":            cp.ModelName,
		"totalTokens":      cp.TokenUsage,
		"conversationID":   cp.Conv.ID.Hex(),
		"conversationTitle": cp.Conv.Title,
	}

	responseJSON, err := json.Marshal(responseObj)
	if err != nil {
		utils.Error("[HandleChat] Error marshaling response", err)
		return
	}

	// Write the response to the client
	fmt.Fprintf(cp.W, "%s\n\n", ([]byte)("data: " + string(responseJSON)))
	cp.W.(http.Flusher).Flush()
}

// saveMessageToDB saves the complete generated response to the database
func (cp *ChunkProcessor) saveMessageToDB(ctx context.Context) {
	_, _, err := db.PushMessage(ctx, cp.Conv, utils.Message{
		Role:      "assistant",
		Timestamp: time.Now().Format(time.RFC3339),
		Content: []utils.MessageContent{
			{
				Type: "text",
				Text: cp.StringProduced,
			},
		},
	})
	
	if err != nil {
		utils.Error("[HandleChat] Error pushing message", err)
	}
}