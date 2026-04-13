package ai

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/azukaar/plurality/src/ai_tools"
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// RunLLMLoop is the autonomous server-side LLM loop. It runs as a goroutine
// that outlives the HTTP connection. It calls the LLM, processes tool calls,
// and broadcasts all events to connected SSE clients.
//
// The loop continues until:
//   - The LLM returns a response with no tool calls (normal completion)
//   - Client-side tool calls are needed (state set to waiting_for_tool)
//   - The context is canceled (user cancellation or timeout)
func (ar *ActiveRequest) RunLLMLoop(ctx context.Context, conversation utils.Conversation, payload ChatPayload) {
	defer ar.cleanup(ctx)

	utils.Log("[LLMLoop] Starting for conversation %s with %d connected clients", ar.ConversationID.Hex(), ar.ClientCount())

	for {
		select {
		case <-ar.Ctx.Done():
			ar.flushPartialResponse(ctx, conversation)
			return
		default:
		}

		model := SelectModel(payload.ModelSelected, conversation.Messages)
		ar.Model = model

		ar.ResetBuffer()
		ar.BroadcastStatus("typing", "")
		utils.Log("[LLMLoop] Calling LLM with model %s", model.Name)
		response, inputPriceToken, err := SendChatCompletion(ar.Ctx, model, conversation, payload)
		if err != nil {
			if ar.Ctx.Err() != nil {
				ar.flushPartialResponse(ctx, conversation)
				return
			}
			utils.Error("[LLMLoop] Error calling LLM", err)
			ar.Broadcast(SSEEvent{
				Type:           "error",
				Content:        err.Error(),
				ConversationID: ar.ConversationID.Hex(),
			})
			ar.setState(ctx, utils.StateIdle)
			ar.BroadcastStatus("", "")
			return
		}

		// Process the streaming response
		sp := NewStreamProcessor(ar, model, conversation, inputPriceToken)
		assistantMessage, err := sp.processStream(ar.Ctx, response, model)
		if err != nil {
			utils.Error("[LLMLoop] Error processing stream", err)
			if ar.Ctx.Err() != nil {
				ar.flushPartialResponse(ctx, conversation)
				return
			}
		}

		utils.Log("[LLMLoop] Stream complete. Text length: %d, Tool calls: %d", len(ar.TextBuffer.String()), len(assistantMessage.ToolCalls))

		// Refresh conversation from DB (finalizeCredits already pushed the assistant message)
		updatedConversation, err := db.GetConversationById(ctx, ar.ConversationID.Hex())
		if err != nil {
			utils.Error("[LLMLoop] Error refreshing conversation from DB", err)
			ar.setState(ctx, utils.StateIdle)
			ar.BroadcastStatus("", "")
			return
		}
		conversation = *updatedConversation

		// If no tool calls, we're done
		if len(assistantMessage.ToolCalls) == 0 {
			utils.Log("[LLMLoop] No tool calls, setting idle and broadcasting done")
			ar.setState(ctx, utils.StateIdle)
			ar.BroadcastStatus("", "")
			ar.Broadcast(SSEEvent{
				Type:           "done",
				ConversationID: ar.ConversationID.Hex(),
				Title:          conversation.Title,
			})

			// Auto-generate title for new conversations
			if conversation.Title == "New Chat" {
				go func() {
					title, icon, err := generateTitleAndIcon(ctx, conversation)
					if err != nil {
						utils.Error("[LLMLoop] Auto title generation failed", err)
						return
					}
					utils.Log("[LLMLoop] Auto-generated title for %s: %s", ar.ConversationID.Hex(), title)
					StatusRegistry.BroadcastToUser(ar.UserID, StatusEvent{
						ConversationID: ar.ConversationID.Hex(),
						State:          string(utils.StateIdle),
						Title:          title,
						Icon:           icon,
					})
				}()
			}

			return
		}

		// Categorize tool calls: server-side vs client-side
		serverTools, clientTools := categorizeToolCalls(assistantMessage.ToolCalls)

		// Execute server-side tools
		for i := range serverTools {
			select {
			case <-ar.Ctx.Done():
				ar.flushPartialResponse(ctx, conversation)
				return
			default:
			}

			tc := &serverTools[i]
			enrichToolCallMetadata(tc)
			ar.BroadcastStatus("tool_use", tc.Function.Name)
			ar.Broadcast(SSEEvent{
				Type:           "tool_use",
				ToolCall:       tc,
				IsServer:       true,
				ConversationID: ar.ConversationID.Hex(),
			})

			resultContent := executeServerTool(ar.Ctx, ar, *tc, payload)

			ar.Broadcast(SSEEvent{
				Type:           "tool_result",
				ToolCallID:     tc.ID,
				ToolName:       tc.Function.Name,
				ToolResult:     resultContent.TextContent(),
				IsServer:       true,
				ConversationID: ar.ConversationID.Hex(),
			})

			toolMessage := utils.Message{
				Role:       "tool",
				Content:    resultContent,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Timestamp:  time.Now().Format(time.RFC3339),
			}
			updatedConv, _, pushErr := db.PushMessage(ctx, conversation, toolMessage)
			if pushErr != nil {
				utils.Error("[LLMLoop] Error saving tool result to DB", pushErr)
			} else {
				conversation = updatedConv
			}
		}

		// If there are client-side tools, pause and wait
		if len(clientTools) > 0 {
			for _, toolCall := range clientTools {
				ar.Broadcast(SSEEvent{
					Type:           "tool_use",
					ToolCall:       &toolCall,
					IsServer:       false,
					ConversationID: ar.ConversationID.Hex(),
				})
			}

			ar.setState(ctx, utils.StateWaitingForTool)
			ar.Broadcast(SSEEvent{
				Type:           "state_change",
				State:          string(utils.StateWaitingForTool),
				ConversationID: ar.ConversationID.Hex(),
			})
			ar.Broadcast(SSEEvent{
				Type:           "done",
				ConversationID: ar.ConversationID.Hex(),
			})
			return // Wait for client to POST tool results
		}

		// All tools were server-side — loop back to LLM with results
		utils.Log("[LLMLoop] All server tools executed, looping back to LLM")
	}
}

// processStream dispatches to the correct provider-specific stream processor.
func (sp *StreamProcessor) processStream(ctx context.Context, response interface{}, model utils.Model) (utils.Message, error) {
	reader, ok := response.(interface{ Close() error })
	if ok {
		_ = reader // just checking the interface
	}

	if strings.HasPrefix(model.Name, "Claude/") {
		return sp.ProcessClaudeStream(ctx, response.(interface {
			Read([]byte) (int, error)
			Close() error
		}))
	} else if strings.HasPrefix(model.Name, "Gemini/") {
		return sp.ProcessGeminiStream(ctx, response.(interface {
			Read([]byte) (int, error)
			Close() error
		}))
	}
	return sp.ProcessStandardStream(ctx, response.(interface {
		Read([]byte) (int, error)
		Close() error
	}))
}

// enrichToolCallMetadata populates the Loading and IconURL fields from the tool registry.
func enrichToolCallMetadata(tc *utils.ToolCall) {
	tool, ok := ai_tools.GetTool(tc.Function.Name)
	if ok {
		tc.Loading = tool.LoadingString
		tc.IconURL = tool.IconURL
	}
}

// categorizeToolCalls splits tool calls into server-side (in registry) and client-side.
func categorizeToolCalls(toolCalls []utils.ToolCall) (serverTools, clientTools []utils.ToolCall) {
	for _, tc := range toolCalls {
		_, isServerTool := ai_tools.GetTool(tc.Function.Name)
		if isServerTool {
			serverTools = append(serverTools, tc)
		} else {
			clientTools = append(clientTools, tc)
		}
	}
	return
}

// executeServerTool runs a server-side tool and handles credit deduction.
func executeServerTool(ctx context.Context, ar *ActiveRequest, toolCall utils.ToolCall, payload ChatPayload) utils.MessageContent {
	tool, ok := ai_tools.GetTool(toolCall.Function.Name)
	if !ok {
		return utils.NewTextContent("Tool not found: " + toolCall.Function.Name)
	}

	args := toolCall.Function.Arguments
	if args == "" {
		args = "{}"
	}

	// Fetch current conversation — all tools receive it
	conv, convErr := db.GetConversationById(ctx, ar.ConversationID.Hex())
	if convErr != nil {
		utils.Error("[LLMLoop] Error loading conversation for tool execution", convErr)
		return utils.NewTextContent("Error: could not load conversation data")
	}

	// Inject user's selected image model into args if applicable
	if tool.ToolID == "generate_image" && payload.ModelSelected.ImageGen != nil && payload.ModelSelected.ImageGen.Name != "" {
		var argsMap map[string]string
		if err := json.Unmarshal([]byte(args), &argsMap); err == nil {
			if argsMap == nil {
				argsMap = make(map[string]string)
			}
			argsMap["model"] = payload.ModelSelected.ImageGen.Name
			if newArgs, err := json.Marshal(argsMap); err == nil {
				args = string(newArgs)
			}
		}
	}

	// Deduct credits
	if tool.CostFunc != nil {
		price, action := tool.CostFunc(args)
		db.RemoveCredits(ctx, price, action)
	} else if tool.Cost > 0 {
		db.RemoveCredits(ctx, float64(tool.Cost), utils.UserAction{Type: TOOL_USE, Provider: NONE})
	}

	return tool.Exec(ctx, args, *conv)
}

// setState updates the conversation state in both the ActiveRequest and the DB.
func (ar *ActiveRequest) setState(ctx context.Context, state utils.ConversationState) {
	ar.State = state
	if err := db.UpdateConversationState(ctx, ar.ConversationID, state); err != nil {
		utils.Error("[LLMLoop] Error updating conversation state", err)
	}
}

// flushPartialResponse saves whatever text was produced so far to the DB on cancellation.
func (ar *ActiveRequest) flushPartialResponse(ctx context.Context, conversation utils.Conversation) {
	text := ar.TextBuffer.String()
	if text == "" {
		ar.setState(ctx, utils.StateIdle)
		ar.BroadcastStatus("", "")
		return
	}

	partialMessage := utils.Message{
		Role:      "assistant",
		Content:   utils.NewTextContent(text),
		Timestamp: time.Now().Format(time.RFC3339),
		Model:     ar.Model,
	}
	db.PushMessage(ctx, conversation, partialMessage)
	ar.setState(ctx, utils.StateIdle)
	ar.BroadcastStatus("", "")
}

// cleanup removes the ActiveRequest from the registry and closes all clients.
func (ar *ActiveRequest) cleanup(ctx context.Context) {
	RequestRegistry.Remove(ar.ConversationID.Hex())
	ar.CloseAllClients()
}
