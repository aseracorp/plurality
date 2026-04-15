package ai

import (
	"bufio"
	"context"
	"encoding/json"
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
	request      *ActiveRequest
	model        utils.Model
	conversation utils.Conversation
}

// NewStreamProcessor creates a StreamProcessor bound to an ActiveRequest.
func NewStreamProcessor(request *ActiveRequest, model utils.Model, conversation utils.Conversation) *StreamProcessor {
	return &StreamProcessor{
		request:      request,
		model:        model,
		conversation: conversation,
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
		ConversationID: sp.request.ConversationID,
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
		for i := range sp.request.ToolCallBuffer {
			enrichToolCallMetadata(&sp.request.ToolCallBuffer[i])
		}
		message.ToolCalls = sp.request.ToolCallBuffer
	}

	return message
}

// finalizeCredits deducts credits and saves the message to DB.
// Uses LiteLLM's response_cost (dollars) converted to credits (1M credits = $1).
// Falls back to manual price table if LiteLLM didn't provide cost.
func (sp *StreamProcessor) finalizeCredits(ctx context.Context) utils.Message {
	provider := ProviderFromModelName(sp.model.Name)

	var totalCredits float64
	if sp.request.ResponseCost > 0 {
		totalCredits = sp.request.ResponseCost * 1_000_000
		utils.Log("[StreamProcessor] LiteLLM cost for %s: $%.6f (%d prompt + %d completion tokens) = %.2f credits",
			sp.model.Name, sp.request.ResponseCost, sp.request.PromptTokens, sp.request.CompletionTokens, totalCredits)
	} else {
		inputPrice := GetPriceFromTokenUsage(TEXT_INPUT, provider, sp.model, float64(sp.request.PromptTokens))
		outputPrice := GetPriceFromTokenUsage(TEXT_OUTPUT, provider, sp.model, float64(sp.request.CompletionTokens))
		totalCredits = inputPrice + outputPrice
		utils.Error("[StreamProcessor] LiteLLM did not return response_cost, falling back to manual price table for model", nil, sp.model.Name)
	}

	// Store the credit cost (not raw token count) so the UI displays the right value
	sp.request.TokenUsage = int(totalCredits)

	message := sp.buildAssistantMessage()

	_, _, err := db.PushMessage(ctx, sp.conversation, message)
	if err != nil {
		utils.Error("[StreamProcessor] Error saving assistant message to DB", err)
	}

	_, err = db.RemoveCredits(ctx, totalCredits, utils.UserAction{
		Type:     TEXT_OUTPUT,
		Provider: provider,
		Model:    sp.model,
	})
	if err != nil {
		utils.MajorError("[StreamProcessor] Error deducting credits", err)
	}

	return message
}

// ProcessStandardStream reads an OpenAI-compatible SSE stream from the LiteLLM proxy.
func (sp *StreamProcessor) ProcessStandardStream(ctx context.Context, response io.ReadCloser) (utils.Message, error) {
	defer response.Close()

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
			return sp.finalizeCredits(ctx), nil
		}

		var chunk AIChunk
		if err := json.Unmarshal([]byte(jsonData), &chunk); err != nil {
			utils.Error("[StreamProcessor] Error parsing chunk", err)
			continue
		}

		// Track actual token usage and cost from LiteLLM
		if chunk.Usage.CompletionTokens > 0 {
			sp.request.CompletionTokens = chunk.Usage.CompletionTokens
		}
		if chunk.Usage.PromptTokens > 0 {
			sp.request.PromptTokens = chunk.Usage.PromptTokens
		}
		if chunk.Usage.ResponseCost > 0 {
			sp.request.ResponseCost = chunk.Usage.ResponseCost
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
