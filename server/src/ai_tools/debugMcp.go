package ai_tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/azukaar/plurality/src/mcp"
	"github.com/azukaar/plurality/src/utils"
)

// DebugMCPTool allows the AI to inspect MCP server logs for debugging.
var DebugMCPTool = utils.AITool{
	Name:              "Debug MCP",
	Description:       "Debug MCP server processes",
	ToolID:            "debug_mcp",
	BundleName:        "mcp_capabilities",
	PickerLabel:       "Debug MCP",
	PickerDescription: "View MCP server logs for debugging",
	PickerDefault:     "ask",
	PickerOrder:       100,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "debug_mcp",
			Description: "Debug MCP server processes. Use 'list' to see available servers, 'logs' to get recent stderr output from a server (useful when MCP tools fail or crash).",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"mode": {
						Type:        "string",
						Description: "'list' for available servers, 'logs' for server stderr",
						Enum:        []string{"list", "logs"},
					},
					"server": {
						Type:        "string",
						Description: "Server name (required for 'logs' mode)",
					},
				},
				Required: []string{"mode"},
			},
		},
	},
	LoadingString: "Checking MCP logs...",
	IconURL:       "",
	Exec:          execDebugMCP,
}

func execDebugMCP(_ context.Context, input string, conv utils.Conversation) utils.MessageContent {
	var params struct {
		Mode   string `json:"mode"`
		Server string `json:"server"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error parsing parameters: %s", err.Error()))
	}

	switch params.Mode {
	case "list":
		servers := mcp.ListAllMCPServers(conv.UserID)
		if len(servers) == 0 {
			return utils.NewTextContent("No MCP servers configured.")
		}
		return utils.NewTextContent(fmt.Sprintf("Available MCP servers: %s", strings.Join(servers, ", ")))

	case "logs":
		if params.Server == "" {
			return utils.NewTextContent("Error: 'server' parameter is required for 'logs' mode.")
		}
		logs := mcp.GetMCPLogs(conv.UserID, params.Server, conv.ID)
		return utils.NewTextContent(fmt.Sprintf("Logs for %s:\n%s", params.Server, logs))

	default:
		return utils.NewTextContent("Invalid mode. Use 'list' or 'logs'.")
	}
}
