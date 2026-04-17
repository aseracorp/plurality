package ai_tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/azukaar/plurality/src/mcp"
	"github.com/azukaar/plurality/src/utils"
)

// ManageMCPTool allows the AI to read and edit the MCP configuration.
var ManageMCPTool = utils.AITool{
	Name:        "Manage MCP",
	Description: "Read and edit MCP server configuration",
	ToolID:      "manage_mcp",
	Cost:        0,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "manage_mcp",
			Description: "Read or edit the MCP server configuration (mcp.json). Use 'get' to view current config, 'set' to replace it entirely. After 'set', MCP servers will be reinitialized.",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"mode": {
						Type:        "string",
						Description: "'get' to read config, 'set' to write new config",
						Enum:        []string{"get", "set"},
					},
					"config": {
						Type:        "string",
						Description: "JSON config for 'set' mode. Must be valid mcp.json format with mcpServers object.",
					},
				},
				Required: []string{"mode"},
			},
		},
	},
	LoadingString: "Managing MCP config...",
	IconURL:       "",
	Exec:          execManageMCP,
}

func execManageMCP(_ context.Context, input string, conv utils.Conversation) utils.MessageContent {
	var params struct {
		Mode   string `json:"mode"`
		Config string `json:"config"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error parsing parameters: %s", err.Error()))
	}

	configPath := mcp.MCPConfigPath()

	switch params.Mode {
	case "get":
		data, err := os.ReadFile(configPath)
		if os.IsNotExist(err) {
			return utils.NewTextContent("No mcp.json configured yet. Use 'set' mode to create one.")
		}
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error reading config: %s", err.Error()))
		}
		return utils.NewTextContent(string(data))

	case "set":
		if params.Config == "" {
			return utils.NewTextContent("Error: 'config' parameter is required for 'set' mode.")
		}

		// Validate JSON structure
		var cfg struct {
			MCPServers map[string]interface{} `json:"mcpServers"`
		}
		if err := json.Unmarshal([]byte(params.Config), &cfg); err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error: invalid JSON: %s", err.Error()))
		}
		if cfg.MCPServers == nil {
			return utils.NewTextContent("Error: config must have 'mcpServers' object.")
		}

		// Pretty-print the config
		formatted, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error formatting config: %s", err.Error()))
		}

		// Write the config
		if err := os.WriteFile(configPath, formatted, 0644); err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error writing config: %s", err.Error()))
		}

		// Reinitialize MCP
		mcp.Shutdown()
		mcp.Init()

		servers := mcp.ListAllMCPServers()
		return utils.NewTextContent(fmt.Sprintf("MCP config updated and reinitialized. Active servers: %v", servers))

	default:
		return utils.NewTextContent("Invalid mode. Use 'get' or 'set'.")
	}
}
