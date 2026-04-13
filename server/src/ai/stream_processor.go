package ai

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// StreamProcessor reads an LLM streaming response and writes events to an ActiveRequest.
// It accumulates text and tool calls, broadcasts SSE events, and returns the
// assembled assistant Message when the stream ends.
type StreamProcessor struct {
	request         *ActiveRequest
	model           utils.Model
	conversation    utils.Conversation
	inputPriceToken int
}

// NewStreamProcessor creates a StreamProcessor bound to an ActiveRequest.
func NewStreamProcessor(request *ActiveRequest, model utils.Model, conversation utils.Conversation, inputPriceToken int) *StreamProcessor {
	return &StreamProcessor{
		request:         request,
		model:           model,
		conversation:    conversation,
		inputPriceToken: inputPriceToken,
	}
}

// broadcastText sends a text chunk to all SSE clients and accumulates it in the buffer.
func (sp *StreamProcessor) broadcastText(content string) {
	if content == "" {
		return
	}
	sp.request.TextBuffer.WriteString(content)
	sp.request.Broadcast(SSEEvent{
		Type:           "text",
		Content:        content,
		ConversationID: sp.request.ConversationID.Hex(),
		Model:          &sp.model,
		TotalTokens:    sp.request.TokenUsage,
		Title:          sp.conversation.Title,
	})
}

// accumulateToolCall collects a new tool call or appends arguments to the last one.
func (sp *StreamProcessor) accumulateToolCall(id, name, arguments string) {
	if name != "" {
		sp.request.ToolCallBuffer = append(sp.request.ToolCallBuffer, utils.ToolCall{
			ID:   id,
			Type: "function",
			Function: utils.FunctionCall{
				Name:      name,
				Arguments: "",
			},
		})
	}
	if arguments != "" && len(sp.request.ToolCallBuffer) > 0 {
		last := &sp.request.ToolCallBuffer[len(sp.request.ToolCallBuffer)-1]
		last.Function.Arguments += arguments
	}
}

// buildAssistantMessage creates the final assistant Message from accumulated buffers.
func (sp *StreamProcessor) buildAssistantMessage() utils.Message {
	message := utils.Message{
		Role:        "assistant",
		Timestamp:   time.Now().Format(time.RFC3339),
		TotalTokens: sp.request.TokenUsage,
		Model:       sp.model,
	}

	text := sp.request.TextBuffer.String()
	if text != "" {
		message.Content = utils.NewTextContent(text)
	}

	if len(sp.request.ToolCallBuffer) > 0 {
		// Enrich tool calls with registry metadata before saving
		for i := range sp.request.ToolCallBuffer {
			enrichToolCallMetadata(&sp.request.ToolCallBuffer[i])
		}
		message.ToolCalls = sp.request.ToolCallBuffer
	}

	return message
}

// finalizeCredits calculates and deducts output credits, saves the message to DB.
func (sp *StreamProcessor) finalizeCredits(ctx context.Context, provider int) utils.Message {
	priceToken := GetPriceFromTokenUsage(TEXT_OUTPUT, provider, sp.model, float64(sp.request.TokenUsage))
	sp.request.TokenUsage = int(priceToken) + sp.inputPriceToken

	message := sp.buildAssistantMessage()

	_, _, err := db.PushMessage(ctx, sp.conversation, message)
	if err != nil {
		utils.Error("[StreamProcessor] Error saving assistant message to DB", err)
	}

	_, err = db.RemoveCredits(ctx, priceToken, utils.UserAction{
		Type:     TEXT_OUTPUT,
		Provider: provider,
		Model:    sp.model,
	})
	if err != nil {
		utils.MajorError("[StreamProcessor] Error deducting output credits", err)
	}

	return message
}

// --- Standard (OpenAI / Fireworks / TogetherAI) ---

// ProcessStandardStream reads an OpenAI-compatible SSE stream.
func (sp *StreamProcessor) ProcessStandardStream(ctx context.Context, response io.ReadCloser) (utils.Message, error) {
	defer response.Close()

	provider := TOGETHER
	if strings.HasPrefix(sp.model.Name, "ChatGPT/") {
		provider = OPENAI
	}

	scanner := bufio.NewScanner(response)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return sp.buildAssistantMessage(), ctx.Err()
		default:
		}

		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		jsonData := strings.TrimPrefix(line, "data: ")

		if jsonData == "[DONE]" {
			if sp.request.TokenUsage == 0 {
				if err, tokenCount := GetTokenNumber(sp.model, sp.request.TextBuffer.String()); err == nil {
					sp.request.TokenUsage = int(tokenCount)
				}
			}
			return sp.finalizeCredits(ctx, provider), nil
		}

		var chunk AIChunk
		if err := json.Unmarshal([]byte(jsonData), &chunk); err != nil {
			utils.Error("[StreamProcessor] Error parsing standard chunk", err)
			continue
		}

		if chunk.Usage.CompletionTokens > 0 {
			sp.request.TokenUsage = chunk.Usage.CompletionTokens
		}
		if chunk.Usage.PromptTokens > 0 {
			priceToken := GetPriceFromTokenUsage(TEXT_INPUT, TOGETHER, sp.model, float64(chunk.Usage.PromptTokens))
			db.RemoveCredits(ctx, priceToken, utils.UserAction{Type: TEXT_INPUT, Provider: TOGETHER, Model: sp.model})
		}

		if len(chunk.Choices) > 0 {
			choice := chunk.Choices[0]
			if choice.Delta.Content != "" {
				sp.broadcastText(choice.Delta.Content)
			} else if choice.Text != "" {
				sp.broadcastText(choice.Text)
			} else if len(choice.Delta.ToolCalls) > 0 {
				tc := choice.Delta.ToolCalls[0]
				sp.accumulateToolCall(tc.ID, tc.Function.Name, tc.Function.Arguments)
			}
		}
	}

	return sp.buildAssistantMessage(), scanner.Err()
}

// --- Claude ---

// ProcessClaudeStream reads a Claude API SSE stream.
func (sp *StreamProcessor) ProcessClaudeStream(ctx context.Context, response io.ReadCloser) (utils.Message, error) {
	defer response.Close()

	scanner := bufio.NewScanner(response)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return sp.buildAssistantMessage(), ctx.Err()
		default:
		}

		line := scanner.Text()
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}

		jsonData := strings.TrimPrefix(line, "data: ")

		var chunk ClaudeAIChunk
		if err := json.Unmarshal([]byte(jsonData), &chunk); err != nil {
			utils.Error("[StreamProcessor] Error parsing Claude chunk", err)
			continue
		}

		if chunk.Usage.OutputTokens > 0 {
			sp.request.TokenUsage = chunk.Usage.OutputTokens
		}
		if chunk.Message.Usage.InputTokens > 0 {
			priceToken := GetPriceFromTokenUsage(TEXT_INPUT, CLAUDE, sp.model, float64(chunk.Message.Usage.InputTokens))
			db.RemoveCredits(ctx, priceToken, utils.UserAction{Type: TEXT_INPUT, Provider: CLAUDE, Model: sp.model})
		}

		switch chunk.Type {
		case "message_stop":
			return sp.finalizeCredits(ctx, CLAUDE), nil

		case "overloaded_error":
			sp.broadcastText("I'm sorry, Claude servers are overloaded right now. Please try again later, or with a different model.")
			return sp.buildAssistantMessage(), fmt.Errorf("claude API overloaded")

		case "content_block_start":
			if chunk.ContentBlock.Type == "tool_use" {
				sp.accumulateToolCall(chunk.ContentBlock.ID, chunk.ContentBlock.Name, "")
			}

		case "content_block_delta":
			if chunk.Delta.Type == "text_delta" && chunk.Delta.Text != "" {
				sp.broadcastText(chunk.Delta.Text)
			} else if chunk.Delta.Type == "input_json_delta" && chunk.Delta.PartialJson != "" {
				if len(sp.request.ToolCallBuffer) > 0 {
					last := &sp.request.ToolCallBuffer[len(sp.request.ToolCallBuffer)-1]
					last.Function.Arguments += chunk.Delta.PartialJson
				}
			}
		}
	}

	return sp.buildAssistantMessage(), scanner.Err()
}

// --- Gemini ---

// ProcessGeminiStream reads a Gemini API SSE stream.
func (sp *StreamProcessor) ProcessGeminiStream(ctx context.Context, response io.ReadCloser) (utils.Message, error) {
	defer response.Close()

	scanner := bufio.NewScanner(response)
	var finalUsage GeminiUsageMetadata

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return sp.buildAssistantMessage(), ctx.Err()
		default:
		}

		line := scanner.Text()
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}

		jsonData := strings.TrimPrefix(line, "data: ")

		var chunk GeminiAIChunk
		if err := json.Unmarshal([]byte(jsonData), &chunk); err != nil {
			utils.Error("[StreamProcessor] Error parsing Gemini chunk", err)
			continue
		}

		if chunk.PromptFeedback != nil && chunk.PromptFeedback.BlockReason != "" {
			sp.broadcastText(fmt.Sprintf("Request blocked: %s", chunk.PromptFeedback.BlockReason))
			return sp.buildAssistantMessage(), fmt.Errorf("gemini request blocked: %s", chunk.PromptFeedback.BlockReason)
		}

		for _, candidate := range chunk.Candidates {
			if candidate.Content == nil {
				continue
			}
			for _, part := range candidate.Content.Parts {
				if part.Text != "" {
					sp.broadcastText(part.Text)
				}
				if part.FunctionCall != nil {
					argsJSON, err := json.Marshal(part.FunctionCall.Args)
					if err != nil {
						utils.Error("[StreamProcessor] Error marshalling Gemini function call args", err)
						continue
					}
					sp.accumulateToolCall(
						fmt.Sprintf("gemini-tool-%d", len(sp.request.ToolCallBuffer)),
						part.FunctionCall.Name,
						string(argsJSON),
					)
				}
			}
		}

		if chunk.UsageMetadata != nil {
			finalUsage = *chunk.UsageMetadata
			if finalUsage.CandidatesTokenCount > 0 {
				sp.request.TokenUsage = finalUsage.CandidatesTokenCount
			}
		}
	}

	// Fallback token counting
	if sp.request.TokenUsage == 0 {
		if finalUsage.TotalTokenCount > 0 && finalUsage.PromptTokenCount > 0 {
			sp.request.TokenUsage = finalUsage.TotalTokenCount - finalUsage.PromptTokenCount
		} else if sp.request.TextBuffer.Len() > 0 {
			if err, tokenCount := GetTokenNumber(sp.model, sp.request.TextBuffer.String()); err == nil {
				sp.request.TokenUsage = int(tokenCount)
			}
		}
	}

	return sp.finalizeCredits(ctx, GOOGLE), scanner.Err()
}

// --- Gemini chunk types (kept here as they're only used by the stream processor) ---

type GeminiAIChunk struct {
	Candidates     []GeminiCandidate     `json:"candidates"`
	PromptFeedback *GeminiPromptFeedback `json:"promptFeedback,omitempty"`
	UsageMetadata  *GeminiUsageMetadata  `json:"usageMetadata,omitempty"`
}

type GeminiCandidate struct {
	Content      *GeminiContent `json:"content"`
	FinishReason string         `json:"finishReason,omitempty"`
	Index        int            `json:"index"`
}

type GeminiContent struct {
	Parts []GeminiPart `json:"parts"`
	Role  string       `json:"role"`
}

type GeminiPart struct {
	Text             string                  `json:"text,omitempty"`
	InlineData       *GeminiInlineData       `json:"inlineData,omitempty"`
	FunctionCall     *GeminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *GeminiFunctionResponse `json:"functionResponse,omitempty"`
}

type GeminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type GeminiFunctionCall struct {
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args"`
}

type GeminiFunctionResponse struct {
	Name     string                 `json:"name"`
	Response map[string]interface{} `json:"response"`
}

type GeminiPromptFeedback struct {
	BlockReason string `json:"blockReason,omitempty"`
}

type GeminiUsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}
