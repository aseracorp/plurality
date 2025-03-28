package ai

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/azukaar/plurality/src/utils"
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/ai_tools"
)

// ChunkProcessor encapsulates the context and methods for processing chunks
type ChunkProcessor struct {
	W               http.ResponseWriter
	Conv            utils.Conversation
	IsNew           bool
	TokenUsage      int
	StringProduced  string
	Model           utils.Model
	InputPriceToken int
	ToolCall        []utils.ToolCallFunction
}

// NewChunkProcessor creates a new ChunkProcessor instance
func NewChunkProcessor(w http.ResponseWriter, conv utils.Conversation, isNew bool, inputPriceToken int, model utils.Model) *ChunkProcessor {
	return &ChunkProcessor{
		W:              w,
		Conv:           conv,
		IsNew:          isNew,
		TokenUsage:     0,
		StringProduced: "",
		InputPriceToken: inputPriceToken,
		Model:			 model,
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

				provider := TOGETHER
				if strings.HasPrefix(cp.Model.Name, "ChatGPT/") {
					provider = OPENAI
				}

				priceToken := GetPriceFromTokenUsage(TEXT_OUTPUT, provider, modelSelected, float64(cp.TokenUsage))
				cp.TokenUsage = int(priceToken) + cp.InputPriceToken
				
				// Push message to DB
				cp.saveMessageToDB(ctx)

				_, err := db.RemoveCredits(ctx, priceToken, utils.UserAction{
					Type: TEXT_OUTPUT,
					Provider: provider,
					Model: modelSelected,
				})
				if err != nil {
					utils.MajorError("[HandleChat] Error removing credits", err)
				}
				
				utils.Log("[HandleChat] Successfully completed chat using %d tokens", cp.TokenUsage)

				// process tool calls
				cp.sendToolChunksToClient()

				cp.sendChunkToClient("")
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

		// cp.ModelName = chunk.Model

		// Extract content from the chunk
		if len(chunk.Choices) > 0 {
			var content string

			// Try to get content from delta first (streaming format)
			if chunk.Choices[0].Delta.Content != "" {
				content = chunk.Choices[0].Delta.Content
			} else if chunk.Choices[0].Text != "" {
				// Fall back to text if available
				content = chunk.Choices[0].Text
			} else if chunk.Choices[0].Delta.ToolCalls != nil && len(chunk.Choices[0].Delta.ToolCalls) > 0 {
				//  tool calls if available
				id := chunk.Choices[0].Delta.ToolCalls[0].ID
				fn := chunk.Choices[0].Delta.ToolCalls[0].Function.Name
			  args := chunk.Choices[0].Delta.ToolCalls[0].Function.Arguments

				// content = "\n" + ai_tools.Registry[fn].LoadingString + "\n"
				//content = fn + args 

				if fn != "" {
					cp.ToolCall = append(cp.ToolCall, utils.ToolCallFunction{
						Name: fn,
						ID: id,
					})
				} else if args != "" {
					if len(cp.ToolCall) > 0 {
						cp.ToolCall[len(cp.ToolCall)-1].Arguments += args
					}
				}
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

		// utils.Debug("[HandleChat] Received Claude chunk %s", line)

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

			priceToken := GetPriceFromTokenUsage(TEXT_OUTPUT, CLAUDE, modelSelected, float64(cp.TokenUsage))
			cp.TokenUsage = int(priceToken) + cp.InputPriceToken
			
			// Push message to DB
			cp.saveMessageToDB(ctx)

			db.RemoveCredits(ctx, priceToken, utils.UserAction{
				Type: TEXT_OUTPUT,
				Provider: CLAUDE,
				Model: modelSelected,
			})
			
			utils.Log("[HandleChat] Successfully completed Claude chat using %d tokens", cp.TokenUsage)
			
			// process tool calls
			cp.sendToolChunksToClient()

			cp.sendChunkToClient("")
			break
		case "overloaded_error":
			utils.Error("[HandleChat] Claude API returned overloaded error", nil)
			cp.sendChunkToClient("I'm sorry, Claude servers are overloaded right now. Please try again later, or with a different model.")
			break
		case "content_block_start":
			// This is the main tool use
			if claudeChunk.ContentBlock.Type == "tool_use" {
				cp.ToolCall = append(cp.ToolCall, utils.ToolCallFunction{
					Name: claudeChunk.ContentBlock.Name,
					ID: claudeChunk.ContentBlock.ID,
				})
			}
		case "content_block_delta":
			// This is the main streaming text from Claude
			if claudeChunk.Delta.Type == "text_delta" && claudeChunk.Delta.Text != "" {
				content := claudeChunk.Delta.Text
				cp.StringProduced += content
				cp.sendChunkToClient(content)
			} else if claudeChunk.Delta.Type == "input_json_delta" && claudeChunk.Delta.PartialJson != "" {
				// This is a tool call from Claude
				// content := claudeChunk.Delta.PartialJson
				// cp.StringProduced += content
				// cp.sendChunkToClient(content)

				// append to arguments of last tool call
				if len(cp.ToolCall) > 0 {
					cp.ToolCall[len(cp.ToolCall)-1].Arguments += claudeChunk.Delta.PartialJson
				}
			}
		}
	}

	return scanner.Err()
}

// sendChunkToClient formats and sends a chunk of text to the client
func (cp *ChunkProcessor) sendChunkToClient(content string) {

	responseObj := map[string]interface{}{
		"type":						 "text",
		"content":          content,
		"model":            cp.Model,
		"totalTokens":      cp.TokenUsage,
		"conversationID":   cp.Conv.ID.Hex(),
		"conversationTitle": cp.Conv.Title,
	}

	responseJSON, err := json.Marshal(responseObj)
	if err != nil {
		utils.Error("[HandleChat] Error marshaling response", err)
		return
	}

	utils.Debug("[HandleChat] Sending response", "data: " + string(responseJSON))

	// Write the response to the client
	fmt.Fprintf(cp.W, "%s\n\n", ([]byte)("data: " + string(responseJSON)))
	cp.W.(http.Flusher).Flush()
}

func (cp *ChunkProcessor) sendToolChunksToClient() {
	for _, tc := range cp.ToolCall {
		tool, _ := ai_tools.GetTool(tc.Name)

		responseObj := map[string]interface{}{
			"type": "tool_use",
			"id": tc.ID,
			"name": tc.Name,
			"arguments": tc.Arguments,
			"loading": tool.LoadingString,
			"icon_url": tool.IconURL,
			"conversationID":   cp.Conv.ID.Hex(),
			"conversationTitle": cp.Conv.Title,
		}

		responseJSON, err := json.Marshal(responseObj)
		if err != nil {
			utils.Error("[HandleChat] Error marshaling response", err)
			return
		}

		utils.Debug("[HandleChat] Sending tool use response", "data: " + string(responseJSON))

		// Write the response to the client
		fmt.Fprintf(cp.W, "%s\n\n", ([]byte)("data: " + string(responseJSON)))
		cp.W.(http.Flusher).Flush()
	}
}

// saveMessageToDB saves the complete generated response to the database
func (cp *ChunkProcessor) saveMessageToDB(ctx context.Context) {
	content := []utils.MessageContent{
		{
			Type: "text",
			Text: cp.StringProduced,
		},
	}

	// process tool use
	for _, tc := range cp.ToolCall {
		tool, _ := ai_tools.GetTool(tc.Name)
		content = append(content, utils.MessageContent{
			Type: "tool_use",
			ToolCall: utils.MessageContentToolCall{
				Name: tc.Name,
				Arguments: tc.Arguments,
				ID: tc.ID,
				Loading: tool.LoadingString,
				IconURL: tool.IconURL,
			},
		})
	}

	_, _, err := db.PushMessage(ctx, cp.Conv, utils.Message{
		TotalTokens: cp.TokenUsage,
		Model:	cp.Model,
		Role:      "assistant",
		Timestamp: time.Now().Format(time.RFC3339),
		Content: content,
	})
	
	if err != nil {
		utils.Error("[HandleChat] Error pushing message", err)
	}
}

// GeminiAIChunk represents a single chunk streamed from the Gemini API
type GeminiAIChunk struct {
	Candidates     []GeminiCandidate  `json:"candidates"`
	PromptFeedback *GeminiPromptFeedback `json:"promptFeedback,omitempty"`
	UsageMetadata  *GeminiUsageMetadata `json:"usageMetadata,omitempty"` // Often in the *last* chunk
}

// GeminiCandidate contains the content generated by the model
type GeminiCandidate struct {
	Content      *GeminiContent `json:"content"`
	FinishReason string         `json:"finishReason,omitempty"` // e.g., "STOP", "MAX_TOKENS", "SAFETY"
	Index        int            `json:"index"`
	// SafetyRatings can be added if needed
}

// GeminiContent holds the parts of the response
type GeminiContent struct {
	Parts []GeminiPart `json:"parts"`
	Role  string       `json:"role"` // Typically "model"
}


type GeminiPart struct {
	Text           string              `json:"text,omitempty"`
	InlineData     *GeminiInlineData   `json:"inlineData,omitempty"`
	FunctionCall   *GeminiFunctionCall `json:"functionCall,omitempty"`
	FunctionResponse *GeminiFunctionResponse `json:"functionResponse,omitempty"`
}

type GeminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"` // Base64 encoded data
}

// GeminiFunctionCall represents a tool call request from Gemini
type GeminiFunctionCall struct {
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args"` // Note: Args is an object, not a string
}

// GeminiPromptFeedback contains safety feedback for the prompt
type GeminiPromptFeedback struct {
	BlockReason string `json:"blockReason,omitempty"`
	// SafetyRatings can be added if needed
}

// GeminiUsageMetadata contains token counts (often in the final chunk)
type GeminiUsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}


// ProcessGeminiChunk processes streamed JSON chunks for Gemini API responses
func (cp *ChunkProcessor) ProcessGeminiChunk(ctx context.Context, response io.ReadCloser, modelSelected utils.Model) error {
	defer response.Close()

	scanner := bufio.NewScanner(response)
	var finalUsage GeminiUsageMetadata // Store the last seen usage data

	utils.Log("[HandleChat GeminiSSE] Starting to scan SSE stream...")

	for scanner.Scan() {
		line := scanner.Text()

		// Skip empty lines or non-data lines (like potential event: lines, though unlikely)
		if line == "" || !strings.HasPrefix(line, "data: ") {
			if line != "" {
				utils.Debug("[HandleChat GeminiSSE] Skipping non-data line: %s", line)
			}
			continue
		}

		// Extract the JSON payload by removing "data: "
		jsonData := strings.TrimPrefix(line, "data: ")
		utils.Debug("[HandleChat GeminiSSE] Received data line content: %s", jsonData)

		// Gemini SSE doesn't typically use "[DONE]", the stream just ends.
		// So, we process every valid data line.

		// Parse the JSON payload into the Gemini structure
		var chunk GeminiAIChunk
		if err := json.Unmarshal([]byte(jsonData), &chunk); err != nil {
			// Log the error and the problematic JSON, but try to continue if possible
			utils.Error("[HandleChat GeminiSSE] Error unmarshalling chunk JSON", err, "JSON data:", jsonData)
			// Depending on the error, you might choose to break or continue
			// If it's invalid JSON, continuing might be difficult.
			continue // Skip this malformed chunk
		}

		// --- Process Chunk Content (Logic borrowed from original ProcessGeminiChunk) ---

		if chunk.PromptFeedback != nil && chunk.PromptFeedback.BlockReason != "" {
			utils.Warn("[HandleChat GeminiSSE] Request blocked", "Reason", chunk.PromptFeedback.BlockReason)
			errMsg := fmt.Sprintf("Request blocked due to: %s", chunk.PromptFeedback.BlockReason)
			cp.sendChunkToClient(errMsg) // Inform client
			// Decide how to handle billing, maybe only input cost?
			// Return an error to stop processing further.
			return fmt.Errorf("gemini request blocked via SSE: %s", chunk.PromptFeedback.BlockReason)
		}

		for _, candidate := range chunk.Candidates {
			if candidate.Content != nil {
				for _, part := range candidate.Content.Parts {
					// Handle Text Part
					if part.Text != "" {
						cp.StringProduced += part.Text
						cp.sendChunkToClient(part.Text)
						utils.Debug("[HandleChat GeminiSSE] Sent text chunk: %s", part.Text)
					}

					// Handle Function Call Part
					if part.FunctionCall != nil {
						fn := part.FunctionCall
						utils.Debug("[HandleChat GeminiSSE] Received function call chunk: %s(%v)", fn.Name, fn.Args)

						argsJSON, err := json.Marshal(fn.Args)
						if err != nil {
							utils.Error("[HandleChat GeminiSSE] Error marshalling function call args", err, fn.Args)
							continue // Skip this tool call if args are bad
						}

						cp.ToolCall = append(cp.ToolCall, utils.ToolCallFunction{
							Name:      fn.Name,
							Arguments: string(argsJSON),
							ID:        fmt.Sprintf("gemini-tool-%d", len(cp.ToolCall)), // Example temporary ID
						})
						// Tool calls are collected and sent at the end by sendToolChunksToClient
					}
				}
			}
			// Check finish reason if needed
			if candidate.FinishReason != "" && candidate.FinishReason != "STOP" {
				utils.Warn("[HandleChat GeminiSSE] Finished with reason", "Reason", candidate.FinishReason)
			}
		}

		// Capture Usage Metadata (often in the last chunk, but check every chunk)
		if chunk.UsageMetadata != nil {
			utils.Debug("[HandleChat GeminiSSE] Received usage metadata: %+v", *chunk.UsageMetadata)
			finalUsage = *chunk.UsageMetadata // Store the latest usage data
			// Update output token count if present
			if finalUsage.CandidatesTokenCount > 0 {
					cp.TokenUsage = finalUsage.CandidatesTokenCount
			}
		}
	} // End of scanner loop

	// --- Post-Stream Processing (After scanner loop finishes) ---

	if err := scanner.Err(); err != nil {
		utils.Error("[HandleChat GeminiSSE] Scanner error", err)
		// Even with scanner error, proceed to finalize based on what was received
		// But return the error at the end.
		// return fmt.Errorf("error scanning Gemini SSE stream: %w", err) // Option to return early
	}

	utils.Log("[HandleChat GeminiSSE] Finished scanning SSE stream.")


	// Fallback token counting if usage metadata wasn't received or incomplete
	if cp.TokenUsage == 0 && finalUsage.TotalTokenCount > 0 && finalUsage.PromptTokenCount > 0 {
		cp.TokenUsage = finalUsage.TotalTokenCount - finalUsage.PromptTokenCount
		utils.Warn("[HandleChat GeminiSSE] Estimated output tokens from total/prompt", "OutputTokens", cp.TokenUsage)
	} else if cp.TokenUsage == 0 && cp.StringProduced != "" { // Only fallback if output was produced
		// Fallback to manual counting (least accurate)
		err, tu := GetTokenNumber(modelSelected, cp.StringProduced) // Ensure GetTokenNumber supports Gemini
		if err != nil {
			utils.MajorError("[HandleChat GeminiSSE] Error getting token number fallback", err)
			cp.TokenUsage = 0 // Or estimate based on characters: len(cp.StringProduced) / 4
		} else {
			cp.TokenUsage = int(tu)
			utils.Warn("[HandleChat GeminiSSE] Used fallback token counter", "OutputTokens", cp.TokenUsage)
		}
	}

	// Calculate final output price
	outputPriceToken := GetPriceFromTokenUsage(TEXT_OUTPUT, GOOGLE, modelSelected, float64(cp.TokenUsage))

	// Store total price (input + output) in TokenUsage
	cp.TokenUsage = cp.InputPriceToken + (int)(outputPriceToken)

	// Save the complete message to DB (ensure saveMessageToDB handles cp.ToolCall)
	cp.saveMessageToDB(ctx)

	// Deduct credits for the output cost
	_, errDb := db.RemoveCredits(ctx, outputPriceToken, utils.UserAction{
		Type:     TEXT_OUTPUT,
		Provider: GOOGLE,
		Model:    modelSelected,
	})
	if errDb != nil {
		utils.MajorError("[HandleChat GeminiSSE] Error removing output credits", errDb)
		// Log error but don't necessarily stop the whole process
	}

	utils.Log("[HandleChat GeminiSSE] Successfully completed chat using %d output tokens (Total Cost Units: %d)", cp.TokenUsage, cp.TokenUsage)

	// Send collected tool call information to the client
	cp.sendToolChunksToClient()

	// Send final empty chunk to signal completion to the client
	cp.sendChunkToClient("")

	// Return scanner error if any occurred during reading
	return scanner.Err()
}