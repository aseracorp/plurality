package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/azukaar/plurality/src/ai_tools"
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/mcp"
	"github.com/azukaar/plurality/src/storage"
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

	utils.Log("[LLMLoop] Starting for conversation %s with %d connected clients", ar.ConversationID, ar.ClientCount())

	for {
		select {
		case <-ar.Ctx.Done():
			ar.flushPartialResponse(ctx, conversation)
			return
		default:
		}

		model := SelectModel(payload.ModelSelected, conversation)
		ar.Model = model

		ar.ResetBuffer()
		ar.BroadcastStatus("typing", "")
		utils.Log("[LLMLoop] Calling LLM with model %s", model.Name)
		response, _, err := SendChatCompletion(ar.Ctx, model, conversation, payload)
		if err != nil {
			if ar.Ctx.Err() != nil {
				ar.flushPartialResponse(ctx, conversation)
				return
			}
			utils.Error("[LLMLoop] Error calling LLM", err)
			ar.Broadcast(SSEEvent{
				Type:           "error",
				Content:        err.Error(),
				ConversationID: ar.ConversationID,
			})
			ar.setState(ctx, utils.StateIdle)
			ar.BroadcastStatus("", "")
			return
		}

		// Process the streaming response (always OpenAI format via LiteLLM)
		sp := NewStreamProcessor(ar, model, conversation)
		assistantMessage, err := sp.ProcessStandardStream(ar.Ctx, response.(io.ReadCloser))
		if err != nil {
			utils.Error("[LLMLoop] Error processing stream", err)
			if ar.Ctx.Err() != nil {
				ar.flushPartialResponse(ctx, conversation)
				return
			}
		}

		utils.Log("[LLMLoop] Stream complete. Text length: %d, Tool calls: %d", len(ar.TextBuffer.String()), len(assistantMessage.ToolCalls))

		// Refresh conversation from DB (finalizeCredits already pushed the assistant message)
		updatedConversation, err := db.GetConversationById(ctx, ar.ConversationID)
		if err != nil {
			utils.Error("[LLMLoop] Error refreshing conversation from DB", err)
			ar.setState(ctx, utils.StateIdle)
			ar.BroadcastStatus("", "")
			return
		}
		conversation = *updatedConversation

		// If no tool calls, we're done
		if len(assistantMessage.ToolCalls) == 0 {
			// If a long_task list still has outstanding items, inject a
			// synthetic tool result that reminds the LLM to keep going.
			// Returns true when a reminder was injected — in that case we
			// loop back to the LLM instead of going idle.
			if injectLongTaskReminder(ctx, ar, &conversation) {
				continue
			}

			utils.Log("[LLMLoop] No tool calls, setting idle and broadcasting done")
			ar.setState(ctx, utils.StateIdle)
			ar.BroadcastStatus("", "")
			ar.Broadcast(SSEEvent{
				Type:           "done",
				ConversationID: ar.ConversationID,
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
					utils.Log("[LLMLoop] Auto-generated title for %s: %s", ar.ConversationID, title)
					StatusRegistry.BroadcastToUser(ar.UserID, StatusEvent{
						ConversationID: ar.ConversationID,
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

		// Split server tools into auto-execute vs needs-approval
		var askServerTools []utils.ToolCall
		if payload.ModelSelected.Text != nil && len(payload.ModelSelected.Text.Tools) > 0 {
			var autoServerTools []utils.ToolCall
			for i := range serverTools {
				mode := payload.ModelSelected.Text.Tools[serverTools[i].Function.Name]
				if mode == "ask" {
					askServerTools = append(askServerTools, serverTools[i])
				} else {
					autoServerTools = append(autoServerTools, serverTools[i])
				}
			}
			serverTools = autoServerTools
		}

		// Execute auto server-side tools
		cancelled := false
		for i := range serverTools {
			tc := &serverTools[i]
			enrichToolCallMetadata(tc)

			var resultContent utils.MessageContent

			select {
			case <-ar.Ctx.Done():
				cancelled = true
				resultContent = utils.NewTextContent("Cancelled by user")
			default:
				ar.BroadcastStatus("tool_use", tc.Function.Name)
				ar.Broadcast(SSEEvent{
					Type:           "tool_use",
					ToolCall:       tc,
					IsServer:       true,
					ConversationID: ar.ConversationID,
				})

				resultContent = executeServerTool(ar.Ctx, ar, *tc, payload)
			}

			// Extract blobs from tool result before saving
			toolMessage := utils.Message{
				Role:       "tool",
				Content:    resultContent,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Timestamp:  time.Now().Format(time.RFC3339),
			}
			if !cancelled {
				if err := storage.ExtractBlobsFromMessage(ar.UserID, &toolMessage); err != nil {
					utils.Error("[LLMLoop] Error extracting blobs from tool result", err)
				}

				ar.Broadcast(SSEEvent{
					Type:           "tool_result",
					ToolCallID:     tc.ID,
					ToolName:       tc.Function.Name,
					ToolResult:     toolMessage.TextContent(),
					IsServer:       true,
					ConversationID: ar.ConversationID,
				})
			}

			updatedConv, _, pushErr := db.PushMessage(ctx, conversation, toolMessage)
			if pushErr != nil {
				utils.Error("[LLMLoop] Error saving tool result to DB", pushErr)
			} else {
				conversation = updatedConv
			}
		}

		// If cancelled, also fill in results for ask-server and client-side tools, then stop
		if cancelled {
			allRemaining := append(askServerTools, clientTools...)
			for i := range allRemaining {
				tc := &allRemaining[i]
				toolMessage := utils.Message{
					Role:       "tool",
					Content:    utils.NewTextContent("Cancelled by user"),
					ToolCallID: tc.ID,
					Name:       tc.Function.Name,
					Timestamp:  time.Now().Format(time.RFC3339),
				}
				updatedConv, _, pushErr := db.PushMessage(ctx, conversation, toolMessage)
				if pushErr != nil {
					utils.Error("[LLMLoop] Error saving cancelled tool result to DB", pushErr)
				} else {
					conversation = updatedConv
				}
			}
			ar.setState(ctx, utils.StateIdle)
			ar.BroadcastStatus("", "")
			return
		}

		// If there are ask-server tools or client-side tools, pause and wait
		if len(askServerTools) > 0 || len(clientTools) > 0 {
			for i := range askServerTools {
				tc := &askServerTools[i]
				enrichToolCallMetadata(tc)
				ar.Broadcast(SSEEvent{
					Type:           "tool_use",
					ToolCall:       tc,
					IsServer:       true,
					ConversationID: ar.ConversationID,
				})
			}
			for _, toolCall := range clientTools {
				ar.Broadcast(SSEEvent{
					Type:           "tool_use",
					ToolCall:       &toolCall,
					IsServer:       false,
					ConversationID: ar.ConversationID,
				})
			}

			// Use waiting_for_approval if any tool (server or client) is in "ask" mode
			waitState := utils.StateWaitingForTool
			hasAskTools := len(askServerTools) > 0
			if !hasAskTools && payload.ModelSelected.Text != nil {
				for _, tc := range clientTools {
					if payload.ModelSelected.Text.Tools[tc.Function.Name] == "ask" {
						hasAskTools = true
						break
					}
				}
			}
			if hasAskTools {
				waitState = utils.StateWaitingForApproval
			}
			ar.setState(ctx, waitState)
			ar.Broadcast(SSEEvent{
				Type:           "state_change",
				State:          string(waitState),
				ConversationID: ar.ConversationID,
			})
			ar.Broadcast(SSEEvent{
				Type:           "done",
				ConversationID: ar.ConversationID,
			})
			return // Wait for client to POST tool results or approvals
		}

		// All tools were server-side — loop back to LLM with results
		utils.Log("[LLMLoop] All server tools executed, looping back to LLM")
	}
}

// enrichToolCallMetadata populates the Loading and IconURL fields from the tool registry.
func enrichToolCallMetadata(tc *utils.ToolCall) {
	tool, ok := ai_tools.GetTool(tc.Function.Name)
	if ok {
		tc.Loading = tool.LoadingString
		tc.IconURL = tool.IconURL
	}
}

// categorizeToolCalls splits tool calls into server-side (server-side MCP or
// builtin registry) and client-side. MCP is checked first so that a namespaced
// MCP tool (e.g. "foo__search_web") is not mistakenly matched to a builtin
// with the same bare name.
func categorizeToolCalls(toolCalls []utils.ToolCall) (serverTools, clientTools []utils.ToolCall) {
	for _, tc := range toolCalls {
		if mcp.IsMCPTool(tc.Function.Name) {
			serverTools = append(serverTools, tc)
			continue
		}
		if _, isBuiltin := ai_tools.GetTool(tc.Function.Name); isBuiltin {
			serverTools = append(serverTools, tc)
			continue
		}
		clientTools = append(clientTools, tc)
	}
	return
}

// executeServerTool runs a server-side tool and handles credit deduction.
// MCP tools are checked first (so namespaced MCP names don't collide with
// builtins); builtin tools come from ai_tools.Registry.
func executeServerTool(ctx context.Context, ar *ActiveRequest, toolCall utils.ToolCall, payload ChatPayload) (result utils.MessageContent) {
	defer func() {
		if r := recover(); r != nil {
			utils.Error(fmt.Sprintf("[LLMLoop] Tool %s panicked: %v", toolCall.Function.Name, r), nil)
			result = utils.NewTextContent(fmt.Sprintf("Error: tool %s encountered an internal error", toolCall.Function.Name))
		}
	}()

	// Check MCP first — namespaced names must match exactly before we
	// try stripping the namespace for a builtin lookup.
	if mcp.IsMCPTool(toolCall.Function.Name) {
		args := toolCall.Function.Arguments
		if args == "" {
			args = "{}"
		}
		return mcp.CallTool(ctx, toolCall.Function.Name, args, ar.ConversationID)
	}

	tool, ok := ai_tools.GetTool(toolCall.Function.Name)
	if !ok {
		return utils.NewTextContent("Tool not found: " + toolCall.Function.Name)
	}

	args := toolCall.Function.Arguments
	if args == "" {
		args = "{}"
	}

	// Fetch current conversation — all tools receive it
	conv, convErr := db.GetConversationById(ctx, ar.ConversationID)
	if convErr != nil {
		utils.Error("[LLMLoop] Error loading conversation for tool execution", convErr)
		return utils.NewTextContent("Error: could not load conversation data")
	}

	// Inject user's selected image model and the fs_read gate into args.
	// The gate flag tells the image tool's Exec it's allowed to honor a 'path'
	// argument — required because the schema-only gate in GetRequests can be
	// bypassed by a hallucinated parameter.
	if tool.ToolID == "generate_image" {
		var argsMap map[string]string
		if err := json.Unmarshal([]byte(args), &argsMap); err == nil {
			if argsMap == nil {
				argsMap = make(map[string]string)
			}
			if payload.ModelSelected.ImageGen != nil && payload.ModelSelected.ImageGen.Name != "" {
				argsMap["model"] = payload.ModelSelected.ImageGen.Name
			}
			if payload.ModelSelected.Text != nil {
				if _, ok := payload.ModelSelected.Text.Tools["filesystem_server"+mcp.NamespaceSeparator+"fs_read"]; ok {
					argsMap["_fs_read_enabled"] = "true"
				}
			}
			if newArgs, err := json.Marshal(argsMap); err == nil {
				args = string(newArgs)
			}
		}
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
	RequestRegistry.Remove(ar.ConversationID)
	ar.CloseAllClients()
}

// injectLongTaskReminder checks the latest long_task state and, when there are
// unfinished tasks and the reminder cap hasn't been hit, appends a synthetic
// assistant + tool message pair to the conversation so the LLM sees the open
// list on the next iteration. Returns true when a reminder was injected.
//
// The synthetic pair is necessary because OpenAI rejects a 'tool' role
// message that doesn't immediately follow an 'assistant' message with a
// matching tool_calls[].id — so we manufacture both halves.
func injectLongTaskReminder(ctx context.Context, ar *ActiveRequest, conv *utils.Conversation) bool {
	state := ai_tools.ReadLongTaskState(conv.Messages)
	if !state.HasOutstanding() {
		return false
	}
	if state.RemindersUsed >= ai_tools.LongTaskMaxReminders {
		utils.Log("[LLMLoop] long_task reminder cap (%d) reached, going idle with %d open task(s)", ai_tools.LongTaskMaxReminders, openCount(state))
		return false
	}

	state.RemindersUsed++
	open := openTitles(state)
	state.Nudge = fmt.Sprintf(
		"You still have %d outstanding task(s): %s. Continue working on them. If they can no longer be completed, call long_task with operation='clear'.",
		len(open),
		strings.Join(open, "; "),
	)
	resultBody := ai_tools.FormatStateJSON(state)

	syntheticID := fmt.Sprintf("longtask_reminder_%d", time.Now().UnixNano())
	tc := utils.ToolCall{
		ID:   syntheticID,
		Type: "function",
		Function: utils.FunctionCall{
			Name:      "long_task",
			Arguments: `{"operation":"_reminder"}`,
		},
	}
	enrichToolCallMetadata(&tc)

	assistantMsg := utils.Message{
		Role:      "assistant",
		ToolCalls: []utils.ToolCall{tc},
		Timestamp: time.Now().Format(time.RFC3339),
	}
	toolMsg := utils.Message{
		Role:       "tool",
		Content:    utils.NewTextContent(resultBody),
		ToolCallID: syntheticID,
		Name:       "long_task",
		Timestamp:  time.Now().Format(time.RFC3339),
	}

	updated, _, err := db.PushMessage(ctx, *conv, assistantMsg)
	if err != nil {
		utils.Error("[LLMLoop] long_task reminder: failed to push assistant msg", err)
		return false
	}
	updated, _, err = db.PushMessage(ctx, updated, toolMsg)
	if err != nil {
		utils.Error("[LLMLoop] long_task reminder: failed to push tool msg", err)
		return false
	}
	*conv = updated

	ar.Broadcast(SSEEvent{
		Type:           "tool_use",
		ToolCall:       &tc,
		IsServer:       true,
		ConversationID: ar.ConversationID,
	})
	ar.Broadcast(SSEEvent{
		Type:           "tool_result",
		ToolCallID:     syntheticID,
		ToolName:       "long_task",
		ToolResult:     resultBody,
		IsServer:       true,
		ConversationID: ar.ConversationID,
	})

	utils.Log("[LLMLoop] long_task reminder injected (%d/%d), %d open task(s)", state.RemindersUsed, ai_tools.LongTaskMaxReminders, len(open))
	return true
}

func openTitles(s ai_tools.LongTaskState) []string {
	out := make([]string, 0, len(s.Tasks))
	for _, t := range s.Tasks {
		if !t.Done {
			out = append(out, t.ID+": "+t.Title)
		}
	}
	return out
}

func openCount(s ai_tools.LongTaskState) int {
	n := 0
	for _, t := range s.Tasks {
		if !t.Done {
			n++
		}
	}
	return n
}
