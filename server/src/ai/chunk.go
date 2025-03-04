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
func (cp *ChunkProcessor) ProcessStandardChunk(ctx context.Context, response io.ReadCloser) error {
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

		// Check for the [DONE] marker
		if jsonData == "[DONE]" {
			if cp.TokenUsage == 0 {
				cp.TokenUsage = strings.Count(cp.StringProduced, " ")
			}

			if cp.IsNew {
				cp.handleNewConversationTitle(ctx)
			}

			fmt.Fprintf(cp.W, "%s\n\n", line)
			cp.W.(http.Flusher).Flush()

			// Push message to DB
			cp.saveMessageToDB(ctx)
			
			utils.Log("[HandleChat] Successfully completed chat using %d tokens", cp.TokenUsage)
			break
		}

		// Parse the JSON
		var chunk AIChunk
		if err := json.Unmarshal([]byte(jsonData), &chunk); err != nil {
			utils.Error("[HandleChat] Error parsing chunk", err, jsonData)
			continue
		}

		if chunk.Usage.TotalTokens > 0 {
			cp.TokenUsage = chunk.Usage.TotalTokens
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
func (cp *ChunkProcessor) ProcessClaudeChunk(ctx context.Context, response io.ReadCloser) error {
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
		// if claudeChunk.Usage.InputTokens > 0 || claudeChunk.Usage.OutputTokens > 0 {
		// 	cp.TokenUsage = claudeChunk.Usage.InputTokens + claudeChunk.Usage.OutputTokens
		// }

		// TODO: output token count in output_tokens

		// Process based on chunk type
		switch claudeChunk.Type {
		case "message_stop":
			if cp.TokenUsage == 0 {
				cp.TokenUsage = strings.Count(cp.StringProduced, " ")
			}

			if cp.IsNew {
				cp.handleNewConversationTitle(ctx)
			}

			fmt.Fprintf(cp.W, "[DONE]\n", line)
			cp.W.(http.Flusher).Flush()

			// Push message to DB
			cp.saveMessageToDB(ctx)
			
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

// handleNewConversationTitle generates and updates the title for new conversations
func (cp *ChunkProcessor) handleNewConversationTitle(ctx context.Context) {
	// Try to generate a title
	firstMessageText := ""
	if len(cp.Conv.Messages) > 0 && len(cp.Conv.Messages[0].Content) > 0 {
		firstMessageText = cp.Conv.Messages[0].Content[0].Text
	}
	
	pp := firstMessageText + " \n Response from AI: " + cp.StringProduced
	if len(pp) > 1024 {
		pp = pp[:1024]
	}
	
	title, err := GenerateTitleForMessage(pp)
	if err != nil {
		utils.Error("[HandleChat] Error generating title", err)
		title = "Unnamed Cookie"
	}
	
	utils.Log("[HandleChat] Generated title for message", title)

	// Send the final response with title
	responseObj := map[string]interface{}{
		"content":          "",
		"model":            cp.ModelName,
		"totalTokens":      cp.TokenUsage,
		"conversationID":   cp.Conv.ID.Hex(),
		"conversationTitle": title,
	}

	responseJSON, err := json.Marshal(responseObj)
	if err == nil {
		fmt.Fprintf(cp.W, "%s\n\n", ([]byte)("data: " + string(responseJSON)))
		cp.W.(http.Flusher).Flush()
	}

	// update title in DB
	cp.Conv.Title = title
	if err := db.UpdateConversationMetadata(ctx, cp.Conv.ID, cp.Conv.Title); err != nil {
		utils.Error("[HandleChat] Error updating conversation", err)
	}
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