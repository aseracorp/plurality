package ai_tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/azukaar/plurality/src/mcp"
	"github.com/azukaar/plurality/src/utils"
)

// ManageMCPTool allows the AI to read and edit the MCP configuration.
var ManageMCPTool = utils.AITool{
	Name:              "Manage MCP",
	Description:       "Read and edit MCP server configuration",
	ToolID:            "manage_mcp",
	BundleName:        "mcp_capabilities",
	PickerLabel:       "Manage MCP",
	PickerDescription: "Read and edit MCP server configuration",
	PickerDefault:     "ask",
	PickerOrder:       90,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "manage_mcp",
			Description: "Read or edit YOUR per-user MCP server configuration (users-data/<you>/mcp.json), layered on top of the shared global servers. Use 'get' to view your current config, 'set' to replace it entirely. After 'set', only your MCP servers are reinitialized.",
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

	configPath := mcp.UserMCPConfigPath(conv.UserID)

	switch params.Mode {
	case "get":
		data, err := os.ReadFile(configPath)
		if os.IsNotExist(err) {
			return utils.NewTextContent("No personal mcp.json configured yet. You currently use the shared global servers only. Use 'set' mode to add your own.")
		}
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error reading config: %s", err.Error()))
		}
		// Credentials must never reach the model. This text becomes an
		// assistant/tool message in the conversation: it is persisted to the
		// conversations DB, re-sent to the provider on every subsequent turn of
		// this conversation, and included in context compaction and search.
		// Returning the file verbatim pastes API keys into all of that.
		return utils.NewTextContent(mcp.RedactSecrets(string(data)))

	case "set":
		if params.Config == "" {
			return utils.NewTextContent("Error: 'config' parameter is required for 'set' mode.")
		}

		// Validate JSON structure. Decoded generically (not into a struct) so
		// the redaction round-trip below can walk it.
		var cfg map[string]interface{}
		if err := json.Unmarshal([]byte(params.Config), &cfg); err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error: invalid JSON: %s", err.Error()))
		}
		if s, ok := cfg["mcpServers"]; !ok || s == nil {
			return utils.NewTextContent("Error: config must have 'mcpServers' object.")
		}

		// 'get' returns secrets redacted, so a config the model round-tripped
		// back through 'set' carries "***REDACTED***" wherever a credential
		// used to be. Writing that verbatim would permanently destroy the
		// user's real keys and brick every server that uses them. Restore the
		// existing values at the matching paths before persisting.
		if prevRaw, err := os.ReadFile(configPath); err == nil {
			var prev map[string]interface{}
			if json.Unmarshal(prevRaw, &prev) == nil {
				if n := mcp.RestoreRedactedSecrets(cfg, prev); n > 0 {
					utils.Log("[MCP] restored %d redacted credential(s) from the existing config", n)
				}
			}
		}

		// Pretty-print the config
		formatted, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error formatting config: %s", err.Error()))
		}

		// Write the config (per-user dir may not exist yet)
		if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error creating user data dir: %s", err.Error()))
		}
		if err := os.WriteFile(configPath, formatted, 0600); err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error writing config: %s", err.Error()))
		}

		// Reinitialize only this user's MCP servers
		mcp.ReinitUser(conv.UserID)

		servers := mcp.ListAllMCPServers(conv.UserID)
		return utils.NewTextContent(fmt.Sprintf("Your MCP config was updated and reinitialized. Servers available to you (incl. global): %v", servers))

	default:
		return utils.NewTextContent("Invalid mode. Use 'get' or 'set'.")
	}
}
