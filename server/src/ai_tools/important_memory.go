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

// UpdateImportantMemoryTool lets the assistant overwrite its own "important
// memory" snippet. The snippet is injected verbatim into the system prompt at
// the start of every chat completion (see ai/index.go), so the tool is
// effectively write-only — the LLM reads the current value from the prompt
// and rewrites the whole thing when it wants to change it.
var UpdateImportantMemoryTool = utils.AITool{
	Name:              "Update Important Memory",
	Description:       "Overwrite the important_memory snippet that is injected into every system prompt.",
	ToolID:            UpdateImportantMemoryToolID,
	Cost:              0,
	PickerLabel:       "Important Memory",
	PickerDescription: "Lets the assistant maintain a compact snippet (the user's name, contacts, key preferences…) that is shown at the top of every conversation.",
	PickerDefault:     "on",
	PickerOrder:       45,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        UpdateImportantMemoryToolID,
			Description: "Overwrite the important_memory snippet that is shown to you at the start of every conversation in the system prompt. The tool is write-only: you cannot read the value back, but the current contents are always visible in the system prompt. Keep it compact — a few short lines. Use it to remember durable facts about the user (name, email, phone, preferences, ongoing projects) so future conversations have the context.",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"content": {
						Type:        "string",
						Description: "The new full content of the snippet. This REPLACES the previous content entirely — include everything you want to keep.",
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
