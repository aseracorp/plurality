package cron

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/azukaar/plurality/src/utils"
)

// Tool is registered with ai_tools from main.go after cron.Init().
var Tool = utils.AITool{
	Name:              "Cron",
	Description:       "Manage scheduled prompts (CRON jobs)",
	ToolID:            "manage_cron",
	PickerLabel:       "Schedules",
	PickerDescription: "Schedule prompts to run autonomously on a CRON expression",
	PickerDefault:     "on",
	PickerOrder:       85,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "manage_cron",
			Description: "Manage scheduled prompts. action=list|create|delete|run|toggle. Before creating, call list_presets to pick a sensible preset_id (defaults to default-background-agent).",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"action":    {Type: "string", Description: "One of: list, create, delete, run, toggle"},
					"schedule":  {Type: "string", Description: "5-field cron expression (m h dom mon dow). Required for create. Optional for update via toggle/etc not supported here."},
					"prompt":    {Type: "string", Description: "Prompt to run when the schedule fires. Required for create."},
					"preset_id": {Type: "string", Description: "Preset/miniapp ID to use. Optional on create (defaults to default-background-agent). Use list_presets to discover IDs."},
					"id":        {Type: "string", Description: "Cron uuid. Required for delete/run/toggle."},
					"enabled":   {Type: "boolean", Description: "Required for toggle: true to enable, false to disable."},
				},
				Required: []string{"action"},
			},
		},
	},
	LoadingString: "Managing schedules",
	Exec: func(ctx context.Context, args string, _ utils.Conversation) utils.MessageContent {
		userID, ok := ctx.Value("userID").(string)
		if !ok || userID == "" {
			return utils.NewTextContent("Error: no user in context")
		}

		var body struct {
			Action   string `json:"action"`
			Schedule string `json:"schedule"`
			Prompt   string `json:"prompt"`
			PresetID string `json:"preset_id"`
			ID       string `json:"id"`
			Enabled  *bool  `json:"enabled"`
		}
		if err := json.Unmarshal([]byte(args), &body); err != nil {
			return utils.NewTextContent("Error parsing args: " + err.Error())
		}

		switch body.Action {
		case "list":
			list, err := LoadAll(userID)
			if err != nil {
				return utils.NewTextContent("Error: " + err.Error())
			}
			data, _ := json.Marshal(list)
			return utils.NewTextContent(string(data))

		case "create":
			job, err := Create(userID, body.Schedule, body.Prompt, body.PresetID)
			if err != nil {
				return utils.NewTextContent("Error: " + err.Error())
			}
			data, _ := json.Marshal(job)
			return utils.NewTextContent("Created cron: " + string(data))

		case "delete":
			if body.ID == "" {
				return utils.NewTextContent("Error: id is required")
			}
			if err := Delete(userID, body.ID); err != nil {
				return utils.NewTextContent("Error: " + err.Error())
			}
			return utils.NewTextContent("Deleted cron " + body.ID)

		case "run":
			if body.ID == "" {
				return utils.NewTextContent("Error: id is required")
			}
			if err := RunNow(userID, body.ID); err != nil {
				return utils.NewTextContent("Error: " + err.Error())
			}
			return utils.NewTextContent("Triggered cron " + body.ID)

		case "toggle":
			if body.ID == "" || body.Enabled == nil {
				return utils.NewTextContent("Error: id and enabled are required")
			}
			job, err := Toggle(userID, body.ID, *body.Enabled)
			if err != nil {
				return utils.NewTextContent("Error: " + err.Error())
			}
			return utils.NewTextContent(fmt.Sprintf("Cron %s now enabled=%v", job.ID, job.Enabled))

		default:
			return utils.NewTextContent("Error: unknown action " + body.Action)
		}
	},
}
