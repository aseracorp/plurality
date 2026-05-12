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

// finalizeStream stores total tokens on the active request and persists the
// assistant message. Tool calls whose accumulated arguments are not valid JSON
// (typically because max_tokens cut the stream mid-call) get their args
// sanitized to "{}" and an explicit failure tool result pushed, so the
// outbound payload to the LLM never carries a malformed or dangling call.
func (sp *StreamProcessor) finalizeStream(ctx context.Context) utils.Message {
	sp.request.TokenUsage = sp.request.PromptTokens + sp.request.CompletionTokens

	message := sp.buildAssistantMessage()

	type failedCall struct{ ID, Name string }
	var failed []failedCall
	for i := range message.ToolCalls {
		tc := &message.ToolCalls[i]
		args := strings.TrimSpace(tc.Function.Arguments)
		if args == "" {
			tc.Function.Arguments = "{}"
			continue
		}
		if !json.Valid([]byte(args)) {
			preview := args
			if len(preview) > 120 {
				preview = preview[:120]
			}
			utils.Log("[StreamProcessor] tool_call %s (%s) args truncated/invalid; failing call. First 120 chars: %s",
				tc.ID, tc.Function.Name, preview)
			tc.Function.Arguments = "{}"
			failed = append(failed, failedCall{ID: tc.ID, Name: tc.Function.Name})
		}
	}

	if updated, _, err := db.PushMessage(ctx, sp.conversation, message); err != nil {
		utils.Error("[StreamProcessor] Error saving assistant message to DB", err)
	} else {
		sp.conversation = updated
	}

	if len(failed) > 0 {
		failBody := "The tool call failed either because the content was not a valid JSON or was too large to fit in a single toolcall. Retry with valid content, or smaller content."
		for _, f := range failed {
			toolMsg := utils.Message{
				Role:       "tool",
				Content:    utils.NewTextContent(failBody),
				ToolCallID: f.ID,
				Name:       f.Name,
				Timestamp:  time.Now().Format(time.RFC3339),
			}
			updatedConv, _, pushErr := db.PushMessage(ctx, sp.conversation, toolMsg)
			if pushErr != nil {
				utils.Error("[StreamProcessor] Error saving failed-call tool result", pushErr)
				continue
			}
			sp.conversation = updatedConv

			sp.request.Broadcast(SSEEvent{
				Type:           "tool_result",
				ToolCallID:     f.ID,
				ToolName:       f.Name,
				ToolResult:     failBody,
				IsServer:       true,
				ConversationID: sp.request.ConversationID,
			})
		}
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
			return sp.finalizeStream(ctx), nil
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
