package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/azukaar/plurality/src/ai"
	"github.com/azukaar/plurality/src/ai_tools"
	"github.com/azukaar/plurality/src/auth"
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/skills"
	"github.com/azukaar/plurality/src/storage"
	"github.com/azukaar/plurality/src/utils"
)

// alwaysOnSubAgentTools returns the set of LLM-facing tool names that the
// registry force-includes for the sub-agent regardless of ModelSelected.Tools,
// given the inherited environment. Listing one of these in the requested
// tools array should be a no-op, not a "dropped" entry.
func alwaysOnSubAgentTools(subMS utils.ModelSelected) map[string]struct{} {
	set := map[string]struct{}{
		ai_tools.WaitTool.ToolID: {}, // unconditional
	}
	if auth.NotificationsEnabled() {
		set[ai_tools.NotifyToolID] = struct{}{}
	}
	if skills.HasAny() {
		set[ai_tools.RetrieveServerSkillTool.ToolID] = struct{}{}
	}
	if subMS.ClientFolderPath != "" {
		set[ai_tools.FsClientReadToolRequest.Function.Name] = struct{}{}
		set[ai_tools.FsClientWriteToolRequest.Function.Name] = struct{}{}
	}
	return set
}

// --- conversations__create_conversation ---

var CreateConversationTool = utils.AITool{
	Name:              "Create Conversation",
	Description:       "Proactively start a new conversation that the user will discover later (AI sends the first message)",
	ToolID:            "create_conversation",
	BundleName:        "conversations",
	Cost:              0,
	PickerLabel:       "Create Conversation",
	PickerDescription: "Let the AI proactively open a new chat with the user",
	PickerDefault:     "on",
	PickerOrder:       90,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "create_conversation",
			Description: "Create a new conversation that starts with an AI message to the user. Use this to surface a new task, reminder, or follow-up outside the current thread. The user will see the new conversation in their list. Returns the new conversation_id so you can reference it later (e.g. via parallel_sub_agent_background_manage).",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"title": {
						Type:        "string",
						Description: "Short title for the new conversation",
					},
					"icon_prompt": {
						Type:        "string",
						Description: "Short visual description used to generate the conversation icon (e.g. 'cozy notebook with a pen')",
					},
					"message": {
						Type:        "string",
						Description: "The first message — sent as if the AI is messaging the user first",
					},
				},
				Required: []string{"title", "icon_prompt", "message"},
			},
		},
	},
	LoadingString: "Creating conversation \"{{title}}\"",
	Exec:          createConversationExec,
}

func createConversationExec(ctx context.Context, args string, conv utils.Conversation) utils.MessageContent {
	parsed := utils.ParseJson(args)
	title, _ := parsed["title"].(string)
	iconPrompt, _ := parsed["icon_prompt"].(string)
	firstMessage, _ := parsed["message"].(string)
	if title == "" || iconPrompt == "" || firstMessage == "" {
		return utils.NewTextContent("Error: title, icon_prompt, and message are all required")
	}

	subCtx := context.WithValue(context.Background(), "userID", conv.UserID)

	msg := utils.Message{
		Role:      "assistant",
		Content:   utils.NewTextContent(firstMessage),
		Timestamp: time.Now().Format(time.RFC3339),
	}
	partial := utils.Conversation{
		Title:         title,
		ModelSelected: conv.ModelSelected,
	}
	updated, _, err := db.PushMessage(subCtx, partial, msg)
	if err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error creating conversation: %v", err))
	}

	if err := db.SetConversationTrigger(subCtx, updated.ID, "conversation", conv.ID); err != nil {
		utils.Error("[create_conversation] SetConversationTrigger failed", err)
	}

	// Tell the sidebar a new (inert) conversation just appeared. The LLM loop
	// isn't running for this one, so the regular "typing" broadcast never
	// fires — clients would otherwise have to refresh manually.
	ai.StatusRegistry.BroadcastToUser(conv.UserID, ai.StatusEvent{
		ConversationID: updated.ID,
		State:          string(utils.StateIdle),
		Title:          title,
	})

	// Generate icon asynchronously — non-fatal if it fails.
	go generateIconAsync(updated.ID, conv.UserID, iconPrompt)

	out := map[string]interface{}{
		"conversation_id": updated.ID,
		"title":           title,
		"status":          "created",
	}
	data, _ := json.Marshal(out)
	return utils.NewTextContent(string(data))
}

func generateIconAsync(convID, userID, iconPrompt string) {
	defer func() {
		if r := recover(); r != nil {
			utils.Error(fmt.Sprintf("[create_conversation] icon goroutine panic: %v", r), nil)
		}
	}()
	fastMS := ai.ShortcutModelSelected("fast")
	imageModel := ""
	if fastMS.ImageGen != nil {
		imageModel = fastMS.ImageGen.Name
	}
	if imageModel == "" {
		return // no image model configured
	}
	prompt := "Simple illustration, visual, " + iconPrompt
	iconData, err := ai.GenerateIconForConversation(prompt, imageModel)
	if err != nil {
		utils.Error("[create_conversation] icon generation failed", err)
		return
	}
	iconURL, err := storage.ExtractIconBlob(userID, iconData)
	if err != nil {
		utils.Error("[create_conversation] save icon failed", err)
		return
	}
	ctx := context.WithValue(context.Background(), "userID", userID)
	if err := db.UpdateConversationMetadata(ctx, convID, "", iconURL); err != nil {
		utils.Error("[create_conversation] persist icon failed", err)
		return
	}
	// Push the icon to any connected status stream clients so the sidebar
	// updates without a manual refresh.
	ai.StatusRegistry.BroadcastToUser(userID, ai.StatusEvent{
		ConversationID: convID,
		State:          string(utils.StateIdle),
		Icon:           iconURL,
	})
}

// --- conversations__parallel_sub_agent ---

var ParallelSubAgentTool = utils.AITool{
	Name:              "Parallel Sub-Agent",
	Description:       "Spawn a sub-agent conversation that processes a prompt",
	ToolID:            "parallel_sub_agent",
	BundleName:        "conversations",
	Cost:              0,
	PickerLabel:       "Parallel Sub-Agent",
	PickerDescription: "Spawn a sub-agent to delegate work, parallelize, or change model tier",
	PickerDefault:     "on",
	PickerOrder:       100,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "parallel_sub_agent",
			Description: "Spawn a sub-agent that processes the prompt as if the user created it. Use this tool to promote model: fast sub-agent to save token or smart to upgrade difficult query. Or use this tool to parallelize tasks, or to spawn sub-agent with less permissions for security. Returns the new conversation_id. When spawning a sub-agent, you cannot grant it more tool permissions than you have yourself — any tool you list that you don't have is dropped and reported back. Background runs survive across server restarts as conversations but their result callback does not (in-memory only). By default the sub-agent's final message is returned to the spawning conversation; set ignore_result='true' to suppress that.",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"prompt": {
						Type:        "string",
						Description: "The prompt to send the sub-agent (as a user message). Be descriptive and clear. It's good practice to nudge the agent toward the right format and tools, especially being explicit about using the long task tool so they don't lose track of their work.",
					},
					"complexity": {
						Type:        "string",
						Description: "Model tier: 'fast' to save tokens, 'medium' for balanced, 'high' to upgrade for a difficult query",
						Enum:        []string{"fast", "medium", "high"},
					},
					"tools": {
						Type:        "string",
						Description: "JSON array of tool names the sub-agent should have (e.g. [\"search_web\",\"conversations__search_conversations\"]). Names must be from your own enabled tool set; others are silently dropped.",
					},
					"background": {
						Type:        "string",
						Description: "'true' to fire and return immediately; 'false' (default) to wait for the sub-agent to finish before returning",
						Enum:        []string{"true", "false"},
					},
					"ignore_result": {
						Type:        "string",
						Description: "'false' (default) to return the sub-agent's final message to the spawning conversation; 'true' to suppress it. With background='true' and ignore_result='false', the result is delivered as a new turn on the parent when the sub-agent finishes.",
						Enum:        []string{"true", "false"},
					},
				},
				Required: []string{"prompt", "complexity", "tools"},
			},
		},
	},
	LoadingString: "Spawning sub-agent",
	Exec:          parallelSubAgentExec,
}

func parallelSubAgentExec(ctx context.Context, args string, conv utils.Conversation) utils.MessageContent {
	parsed := utils.ParseJson(args)
	prompt, _ := parsed["prompt"].(string)
	complexity, _ := parsed["complexity"].(string)
	if prompt == "" || complexity == "" {
		return utils.NewTextContent("Error: prompt and complexity are required")
	}

	requestedTools := parseToolsArg(parsed["tools"])
	background := parseBoolArg(parsed["background"], false)
	ignoreResult := parseBoolArg(parsed["ignore_result"], false)

	// Map complexity → config shortcut name.
	shortcut := ""
	switch complexity {
	case "fast":
		shortcut = "fast"
	case "medium":
		shortcut = "medium"
	case "high", "smart":
		shortcut = "smart"
	default:
		return utils.NewTextContent("Error: complexity must be one of fast, medium, high")
	}

	// Build the sub-agent's ModelSelected from the shortcut, then overwrite
	// Text.Tools with the intersection of requested tools and parent's enabled tools.
	subMS := ai.ShortcutModelSelected(shortcut)
	// Inherit the parent's attached client folder so the sub-agent can act on
	// the same workspace. filesystem_client tool schemas are force-added at
	// request time (registry.go) when ClientFolderPath is non-empty.
	subMS.ClientFolderPath = conv.ModelSelected.ClientFolderPath
	parentTools := map[string]string{}
	if conv.ModelSelected.Text != nil {
		parentTools = conv.ModelSelected.Text.Tools
	}
	allowed := map[string]string{}
	var dropped []string
	alwaysOn := alwaysOnSubAgentTools(subMS)
	for _, t := range requestedTools {
		if _, ok := alwaysOn[t]; ok {
			continue // force-included by registry; no need to opt in or report dropped
		}
		mode, ok := parentTools[t]
		if !ok || (mode != "true" && mode != "ask") {
			dropped = append(dropped, t)
			continue
		}
		allowed[t] = "true" // sub-agent auto-executes; no user to approve
	}
	if subMS.Text == nil {
		subMS.Text = &utils.Model{}
	}
	subMS.Text.Tools = allowed
	if subMS.Vision != nil {
		subMS.Vision.Tools = allowed
	}

	title := prompt
	prompt = "You are a sub-agent spawned by the parent conversation to handle: " + prompt

	subID, done, err := RunSubAgent(ctx, conv.UserID, SubAgentOptions{
		Prompt:        prompt,
		Title:         title,
		ParentID:      conv.ID,
		ModelSelected: subMS,
	})
	if err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error spawning sub-agent: %v", err))
	}

	out := map[string]interface{}{
		"conversation_id": subID,
	}
	if len(dropped) > 0 {
		out["dropped_tools"] = dropped
	}

	if background {
		out["status"] = "running"
		if !ignoreResult {
			parentID := conv.ID
			userID := conv.UserID
			go func() {
				<-done
				delivered := readLastAssistantText(subID, userID)
				if delivered == "" {
					delivered = "(sub-agent produced no assistant message)"
				}
				content := fmt.Sprintf("[sub-agent %s completed]\n%s", subID, delivered)
				if err := ai.InjectAndReinvoke(parentID, userID, content); err != nil {
					utils.Error("[parallel_sub_agent] InjectAndReinvoke failed", err)
				}
			}()
		}
		data, _ := json.Marshal(out)
		return utils.NewTextContent(string(data))
	}

	// Blocking — wait for completion or parent cancellation.
	select {
	case <-done:
	case <-ctx.Done():
		if subAR := ai.RequestRegistry.Get(subID); subAR != nil {
			subAR.Cancel()
		}
		out["status"] = "cancelled"
		data, _ := json.Marshal(out)
		return utils.NewTextContent(string(data))
	}

	out["status"] = "done"
	if !ignoreResult {
		out["result"] = readLastAssistantText(subID, conv.UserID)
	}
	data, _ := json.Marshal(out)
	return utils.NewTextContent(string(data))
}

// --- conversations__parallel_sub_agent_background_manage ---

var ManageSubAgentTool = utils.AITool{
	Name:              "Manage Sub-Agents",
	Description:       "List, stop, or follow up on background sub-agents spawned by this conversation",
	ToolID:            "parallel_sub_agent_background_manage",
	BundleName:        "conversations",
	Cost:              0,
	PickerLabel:       "Manage Sub-Agents",
	PickerDescription: "Manage background sub-agents (list, stop, follow up)",
	PickerDefault:     "on",
	PickerOrder:       110,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "parallel_sub_agent_background_manage",
			Description: "Manage background sub-agents you have spawned from this conversation. Operations: 'list' (no other args needed) returns id/title/state/last assistant text of each child; 'stop' cancels a running child (requires conversation_id); 'follow_up' sends a follow-up user message to an idle child (requires conversation_id and message). You can only manage sub-agents that you spawned.",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"operation": {
						Type:        "string",
						Description: "Operation to perform",
						Enum:        []string{"list", "stop", "follow_up"},
					},
					"conversation_id": {
						Type:        "string",
						Description: "Sub-agent's conversation ID. Required for 'stop' and 'follow_up'.",
					},
					"message": {
						Type:        "string",
						Description: "Follow-up user message. Required for 'follow_up'.",
					},
				},
				Required: []string{"operation"},
			},
		},
	},
	LoadingString: "Managing sub-agents",
	Exec:          manageSubAgentExec,
}

func manageSubAgentExec(ctx context.Context, args string, conv utils.Conversation) utils.MessageContent {
	parsed := utils.ParseJson(args)
	operation, _ := parsed["operation"].(string)

	switch operation {
	case "list":
		return listSubAgents(conv)
	case "stop":
		convID, _ := parsed["conversation_id"].(string)
		if convID == "" {
			return utils.NewTextContent("Error: conversation_id required for stop")
		}
		return stopSubAgent(conv, convID)
	case "follow_up":
		convID, _ := parsed["conversation_id"].(string)
		message, _ := parsed["message"].(string)
		if convID == "" || message == "" {
			return utils.NewTextContent("Error: conversation_id and message required for follow_up")
		}
		return followUpSubAgent(conv, convID, message)
	default:
		return utils.NewTextContent("Error: operation must be one of list, stop, follow_up")
	}
}

func listSubAgents(parent utils.Conversation) utils.MessageContent {
	ctx := context.WithValue(context.Background(), "userID", parent.UserID)
	children, err := db.ListConversationsByTrigger(ctx, "sub_agent", parent.ID)
	if err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error listing: %v", err))
	}
	out := make([]map[string]interface{}, 0, len(children))
	for _, c := range children {
		entry := map[string]interface{}{
			"conversation_id": c.ID,
			"title":           c.Title,
			"state":           string(c.State),
			"last_message_at": c.LastMessageAt.Format(time.RFC3339),
		}
		// Load last assistant text — cheap enough; could batch later if it matters.
		entry["last_assistant_text"] = readLastAssistantText(c.ID, parent.UserID)
		out = append(out, entry)
	}
	data, _ := json.Marshal(map[string]interface{}{"sub_agents": out, "count": len(out)})
	return utils.NewTextContent(string(data))
}

func stopSubAgent(parent utils.Conversation, subID string) utils.MessageContent {
	if err := verifyChildOwnership(parent, subID); err != nil {
		return utils.NewTextContent(err.Error())
	}
	if ar := ai.RequestRegistry.Get(subID); ar != nil {
		ar.Cancel()
	}
	ctx := context.WithValue(context.Background(), "userID", parent.UserID)
	if err := db.UpdateConversationState(ctx, subID, utils.StateIdle); err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error setting state idle: %v", err))
	}
	data, _ := json.Marshal(map[string]string{"conversation_id": subID, "status": "stopped"})
	return utils.NewTextContent(string(data))
}

func followUpSubAgent(parent utils.Conversation, subID, message string) utils.MessageContent {
	if err := verifyChildOwnership(parent, subID); err != nil {
		return utils.NewTextContent(err.Error())
	}
	ctx := context.WithValue(context.Background(), "userID", parent.UserID)
	child, err := db.GetConversationById(ctx, subID)
	if err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error: %v", err))
	}
	if child.State != utils.StateIdle {
		return utils.NewTextContent(fmt.Sprintf("Error: sub-agent %s is not idle (state=%s); follow-up only allowed when idle", subID, child.State))
	}
	if err := ai.InjectAndReinvoke(subID, parent.UserID, message); err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error sending follow-up: %v", err))
	}
	data, _ := json.Marshal(map[string]string{"conversation_id": subID, "status": "follow_up_sent"})
	return utils.NewTextContent(string(data))
}

func verifyChildOwnership(parent utils.Conversation, subID string) error {
	ctx := context.WithValue(context.Background(), "userID", parent.UserID)
	child, err := db.GetConversationById(ctx, subID)
	if err != nil {
		return fmt.Errorf("Error: sub-agent %s not found", subID)
	}
	if child.TriggerType != "sub_agent" || child.TriggerID != parent.ID {
		return fmt.Errorf("Error: %s is not a sub-agent of this conversation", subID)
	}
	return nil
}

// --- helpers ---

func parseToolsArg(raw interface{}) []string {
	switch v := raw.(type) {
	case nil:
		return nil
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return nil
		}
		// Try JSON array first.
		var arr []string
		if err := json.Unmarshal([]byte(s), &arr); err == nil {
			return arr
		}
		// Fall back to comma-separated.
		parts := strings.Split(s, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	return nil
}

func parseBoolArg(raw interface{}, defaultVal bool) bool {
	switch v := raw.(type) {
	case nil:
		return defaultVal
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		if s == "" {
			return defaultVal
		}
		return s == "true" || s == "1" || s == "yes"
	}
	return defaultVal
}

func readLastAssistantText(convID, userID string) string {
	ctx := context.WithValue(context.Background(), "userID", userID)
	c, err := db.GetConversationById(ctx, convID)
	if err != nil {
		return ""
	}
	for i := len(c.Messages) - 1; i >= 0; i-- {
		if c.Messages[i].Role == "assistant" {
			return c.Messages[i].TextContent()
		}
	}
	return ""
}
