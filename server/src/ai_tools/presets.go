package ai_tools

import (
	"context"
	"encoding/json"

	"github.com/azukaar/plurality/src/miniapps"
	"github.com/azukaar/plurality/src/utils"
)

var ListPresetsTool = utils.AITool{
	Name:              "List Presets",
	Description:       "List available preset IDs and names",
	ToolID:            "list_presets",
	PickerLabel:       "List Presets",
	PickerDescription: "Discover available presets (system prompts) — useful before scheduling a CRON",
	PickerDefault:     "on",
	PickerOrder:       80,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "list_presets",
			Description: "Lists available preset/miniapp IDs, names, and descriptions for the current user. Call this before creating a scheduled CRON so you can pick a sensible preset_id.",
			Parameters: &utils.ParameterToolsRequest{
				Type:       "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{},
			},
		},
	},
	LoadingString: "Listing presets",
	Exec: func(ctx context.Context, _ string, _ utils.Conversation) utils.MessageContent {
		userID, _ := ctx.Value("userID").(string)
		apps := miniapps.ListForUser(userID)
		type entry struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		out := make([]entry, 0, len(apps))
		for _, a := range apps {
			out = append(out, entry{ID: a.ID, Name: a.Name, Description: a.Description})
		}
		data, err := json.Marshal(out)
		if err != nil {
			return utils.NewTextContent("Error listing presets: " + err.Error())
		}
		return utils.NewTextContent(string(data))
	},
}
