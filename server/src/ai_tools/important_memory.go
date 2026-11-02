package ai_tools

import (
	"context"
	"encoding/json"

	"github.com/azukaar/plurality/src/memory"
	"github.com/azukaar/plurality/src/utils"
)

const UpdateImportantMemoryToolID = "update_important_memory"

func memoryToolError(msg string) utils.MessageContent {
	b, _ := json.Marshal(map[string]string{"status": "error", "error": msg})
	return utils.NewTextContent(string(b))
}

// UpdateImportantMemoryTool lets the assistant overwrite the user's GLOBAL
// durable memory snippet (name, contacts, preferences, standing rules).
// It is injected into the system prompt of EVERY conversation, so it must
// stay compact and durable — never per-conversation task state.
var UpdateImportantMemoryTool = utils.AITool{
	Name:              "Update Important Memory",
	Description:       "Overwrite the user's GLOBAL important_memory snippet - the only memory that is visible in EVERY conversation. It must contain only durable facts about the user that apply to ALL conversations (name, email, phone, preferences, standing constraints, high-level stable projects).",
	ToolID:            UpdateImportantMemoryToolID,
	PickerLabel:       "Important Memory",
	PickerDescription: "Maintain the user's GLOBAL memory: durable facts (name, contacts, preferences, standing rules) shown in every conversation. NOT for task progress - use the task list / long_task tool inside the conversation for that.",
	PickerDefault:     "on",
	PickerOrder:       45,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name: UpdateImportantMemoryToolID,
			Description: `Overwrite the GLOBAL important_memory snippet that is injected verbatim into the system prompt of EVERY conversation. The tool is write-only: you cannot read the value back, but the current contents are always visible in the system prompt.

Store ONLY durable facts about the user that apply to ALL conversations: name, email, phone, address, preferences, standing constraints/rules, stable high-level projects (e.g. 'homelab with pve01/pve02'). Keep it to a few short lines.

NEVER store in here: task progress (DONE / NEXT / TODO / CUTOVER steps), current working state, timestamps or clocks ('current time...'), step-by-step commands, file/infra details of one specific job, or anything that only matters to the conversation you are in right now. That kind of state belongs in the conversation itself - use the long_task checklist for todo steps and leave the details in the chat history. If what you are about to write is mostly task state, DO NOT update important_memory; keep it in the conversation instead.

Before overwriting, preserve anything still true from the previous content that you want to keep (durable facts only); drop task-specific and time-specific lines.`,
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"content": {
						Type:        "string",
						Description: "The new full content of the GLOBAL memory. This REPLACES the previous content entirely. Durable user facts only - no task progress, no timestamps, no per-conversation state.",
					},
				},
				Required: []string{"content"},
			},
		},
	},
	LoadingString: "Updating important memory",
	Exec: func(ctx context.Context, args string, _ utils.Conversation) utils.MessageContent {
		var p struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(args), &p); err != nil {
			return memoryToolError("invalid arguments: " + err.Error())
		}
		userID, _ := ctx.Value("userID").(string)
		if userID == "" {
			return memoryToolError("no user in context")
		}
		if err := memory.Set(userID, p.Content); err != nil {
			return memoryToolError(err.Error())
		}
		out, _ := json.Marshal(map[string]string{"status": "updated"})
		return utils.NewTextContent(string(out))
	},
}
