package webhook

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/azukaar/plurality/src/utils"
)

// PublicURL is the base URL the AI tool reports back in URLs. The trigger
// endpoint lives at `${PublicURL}/webhook/{id}?token=...`. Set this from
// main if you have a known external URL; otherwise the tool falls back to
// the empty string and the caller can substitute their own scheme/host.
var PublicURL string

func toolURL(id, token string) string {
	if PublicURL == "" {
		return "/webhook/" + id + "?token=" + token
	}
	return PublicURL + "/webhook/" + id + "?token=" + token
}

// Tool is registered with ai_tools from main.go after webhook.Init().
var Tool = utils.AITool{
	Name:              "Webhook",
	Description:       "Manage HTTP-triggered prompts (webhooks)",
	ToolID:            "manage_webhook",
	PickerLabel:       "Webhooks",
	PickerDescription: "Trigger prompts via an external HTTP request",
	PickerDefault:     "on",
	PickerOrder:       86,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name: "manage_webhook",
			Description: "Manage HTTP-triggered prompts. action=list|create|delete|toggle|rotate_token. " +
				"create and rotate_token return the URL and plaintext token EXACTLY ONCE — surface them " +
				"to the user immediately. The caller can pass the token via ?token=... OR " +
				"X-WEBHOOK-TOKEN header on the trigger request.",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"action":          {Type: "string", Description: "One of: list, create, delete, toggle, rotate_token"},
					"prompt":          {Type: "string", Description: "Prompt to run when the webhook fires. Required for create."},
					"preset_id":       {Type: "string", Description: "Preset/miniapp ID. Optional on create (defaults to default-background-agent). Use list_presets to discover IDs."},
					"id":              {Type: "string", Description: "Webhook uuid. Required for delete/toggle/rotate_token."},
					"enabled":         {Type: "boolean", Description: "Required for toggle: true to enable, false to disable."},
					"conversation_id": {Type: "string", Description: "Optional. When set, each trigger appends to that conversation instead of creating a new one. Pass '1' as a sentinel to start a new persistent thread that the first trigger will materialise."},
				},
				Required: []string{"action"},
			},
		},
	},
	LoadingString: "Managing webhooks",
	Exec: func(ctx context.Context, args string, _ utils.Conversation) utils.MessageContent {
		userID, ok := ctx.Value("userID").(string)
		if !ok || userID == "" {
			return utils.NewTextContent("Error: no user in context")
		}

		var body struct {
			Action         string `json:"action"`
			Prompt         string `json:"prompt"`
			PresetID       string `json:"preset_id"`
			ID             string `json:"id"`
			Enabled        *bool  `json:"enabled"`
			ConversationID string `json:"conversation_id"`
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
			out := make([]PublicWebhook, 0, len(list))
			for _, w := range list {
				out = append(out, w.Public())
			}
			data, _ := json.Marshal(out)
			return utils.NewTextContent(string(data))

		case "create":
			hook, token, err := Create(userID, body.Prompt, body.PresetID, body.ConversationID)
			if err != nil {
				return utils.NewTextContent("Error: " + err.Error())
			}
			resp := WebhookCreateResponse{
				PublicWebhook: hook.Public(),
				URL:           toolURL(hook.ID, token),
				Token:         token,
			}
			data, _ := json.Marshal(resp)
			return utils.NewTextContent("Created webhook (save this token — it won't be shown again): " + string(data))

		case "delete":
			if body.ID == "" {
				return utils.NewTextContent("Error: id is required")
			}
			if err := Delete(userID, body.ID); err != nil {
				return utils.NewTextContent("Error: " + err.Error())
			}
			return utils.NewTextContent("Deleted webhook " + body.ID)

		case "toggle":
			if body.ID == "" || body.Enabled == nil {
				return utils.NewTextContent("Error: id and enabled are required")
			}
			hook, err := Toggle(userID, body.ID, *body.Enabled)
			if err != nil {
				return utils.NewTextContent("Error: " + err.Error())
			}
			return utils.NewTextContent(fmt.Sprintf("Webhook %s now enabled=%v", hook.ID, hook.Enabled))

		case "rotate_token":
			if body.ID == "" {
				return utils.NewTextContent("Error: id is required")
			}
			hook, token, err := RotateToken(userID, body.ID)
			if err != nil {
				return utils.NewTextContent("Error: " + err.Error())
			}
			resp := WebhookCreateResponse{
				PublicWebhook: hook.Public(),
				URL:           toolURL(hook.ID, token),
				Token:         token,
			}
			data, _ := json.Marshal(resp)
			return utils.NewTextContent("Rotated token (the old one no longer works): " + string(data))

		default:
			return utils.NewTextContent("Error: unknown action " + body.Action)
		}
	},
}
