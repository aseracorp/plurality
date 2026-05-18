package ai_tools

import (
	"fmt"
	"strings"

	"github.com/azukaar/plurality/src/auth"
	"github.com/azukaar/plurality/src/mcp"
	"github.com/azukaar/plurality/src/skills"
	"github.com/azukaar/plurality/src/utils"
)

var Registry = map[string]utils.AITool{
	DiceRollTool.ToolID:                DiceRollTool,
	WebTool.ToolID:                     WebTool,
	SearchTool.ToolID:                  SearchTool,
	PlaceSearchTool.ToolID:             PlaceSearchTool,
	ImageGenTool.ToolID:                ImageGenTool,
	ConversationAttachmentsTool.ToolID: ConversationAttachmentsTool,
	ReadDocumentTool.ToolID:            ReadDocumentTool,
	SearchDocumentTool.ToolID:          SearchDocumentTool,
	SearchConversationsTool.ToolID:     SearchConversationsTool,
	RetrieveConversationTool.ToolID:    RetrieveConversationTool,
	DebugMCPTool.ToolID:                DebugMCPTool,
	ManageMCPTool.ToolID:               ManageMCPTool,
	ShellExecTool.ToolID:               ShellExecTool,
	AptInstallTool.ToolID:              AptInstallTool,
	FsServerReadTool.ToolID:            FsServerReadTool,
	FsServerWriteTool.ToolID:           FsServerWriteTool,
	LongTaskTool.ToolID:                LongTaskTool,
	ListPresetsTool.ToolID:             ListPresetsTool,
	WaitTool.ToolID:                    WaitTool,
	NotifyTool.ToolID:                  NotifyTool,
	UpdateImportantMemoryTool.ToolID:   UpdateImportantMemoryTool,
}

// RegisterRetrieveServerSkill adds retrieve_server_skill to the registry.
// Called from main after skills.Init(), only when at least one skill exists.
func RegisterRetrieveServerSkill() {
	Registry[RetrieveServerSkillTool.ToolID] = RetrieveServerSkillTool
}

func RegisterTool(tool utils.AITool) {
	Registry[tool.ToolID] = tool
}

func GetTool(toolID string) (utils.AITool, bool) {
	tool, ok := Registry[toolID]
	if ok {
		return tool, true
	}
	// Strip namespace prefix for bundled builtins (e.g.
	// "conversations__search_conversations" → "search_conversations"), but
	// ONLY when the prefix matches the resolved tool's BundleName. Otherwise
	// schema-only namespaced tools like "filesystem_client__fs_read" would
	// accidentally match an unrelated server tool with the same bare ID
	// (FsServerReadTool.ToolID == "fs_read"), get categorized as a
	// server-side tool, and be executed by the server instead of forwarded
	// to the client.
	if idx := strings.Index(toolID, mcp.NamespaceSeparator); idx >= 0 {
		prefix := toolID[:idx]
		bare := toolID[idx+len(mcp.NamespaceSeparator):]
		if candidate, ok := Registry[bare]; ok && candidate.BundleName == prefix {
			return candidate, true
		}
	}
	return utils.AITool{}, false
}

func ShouldStripResponse(content string) bool {
	return strings.HasPrefix(content, "base64,")
}

func GetRequests(model utils.Model, ClientSideTools []utils.FunctionToolsRequest, hasAttachments bool, hasDocumentAttachments bool, hasClientFolder bool) []utils.ToolsRequest {
	var requests []utils.ToolsRequest
	var selected = model.Tools

	for _, tool := range Registry {
		if tool.ToolID == ConversationAttachmentsTool.ToolID || tool.ToolID == ReadDocumentTool.ToolID || tool.ToolID == SearchDocumentTool.ToolID {
			continue // handled separately below
		}
		if tool.ToolID == RetrieveServerSkillTool.ToolID {
			continue // force-included below when skills exist
		}
		if tool.ToolID == WaitTool.ToolID {
			continue // force-included unconditionally below
		}
		if tool.ToolID == NotifyTool.ToolID {
			continue // force-included below only when NTFY is configured
		}

		// Build the selection key: namespaced for bundled tools, bare for standalone.
		selKey := tool.ToolID
		if tool.BundleName != "" {
			selKey = tool.BundleName + mcp.NamespaceSeparator + tool.ToolID
		}

		if _, ok := selected[selKey]; ok {
			req := tool.ToolRequest
			if tool.BundleName != "" {
				// Emit the namespaced name and enriched description to the LLM.
				req.Function.Name = selKey
				req.Function.Description = fmt.Sprintf("[%s] %s", tool.BundleName, req.Function.Description)
			}
			// When the file read tool is also enabled, expose a 'path' option on
			// the image tool so agents can edit images directly off the server FS.
			// When it's not enabled, hint that the capability exists so the LLM
			// can ask the user to enable it.
			if tool.ToolID == ImageGenTool.ToolID {
				if _, fsReadEnabled := selected["filesystem_server"+mcp.NamespaceSeparator+"fs_read"]; fsReadEnabled {
					// Deep-copy Properties — req is a shallow copy of the registry
					// entry and shares the underlying map.
					oldProps := req.Function.Parameters.Properties
					newProps := make(map[string]utils.PropertyParameterToolsRequest, len(oldProps)+1)
					for k, v := range oldProps {
						newProps[k] = v
					}
					newProps["path"] = utils.PropertyParameterToolsRequest{
						Type:        "string",
						Description: "Optional absolute or '~/'-prefixed server file path to load as the source image for editing. Mutually exclusive with 'attachment'.",
					}
					newParams := *req.Function.Parameters
					newParams.Properties = newProps
					req.Function.Parameters = &newParams
					req.Function.Description += " You may also pass 'path' to load a server-side file as the source image (instead of an 'attachment' ID)."
				} else {
					req.Function.Description += " (Note: editing an image directly from a server file path is also supported, but requires the user to enable the 'Read Files (Server)' tool — ask the user to enable it if they want to edit a file by path.)"
				}
			}
			requests = append(requests, req)
		}
	}

	// Server-side MCP tools (from data/mcp.json). Names are already
	// namespaced (serverName__toolName) by mcp.ToolsRequests().
	for _, mcpReq := range mcp.ToolsRequests() {
		if _, ok := selected[mcpReq.Function.Name]; ok {
			requests = append(requests, mcpReq)
		}
	}

	// Client-side tools (MCP tools from Flutter, skills). MCP tools arrive
	// already namespaced from the client.
	for _, tool := range ClientSideTools {
		if _, ok := selected[tool.Name]; ok {
			requests = append(requests, utils.ToolsRequest{
				Type:     "function",
				Function: tool,
			})
		}
	}

	// Force-include conversation_attachments when the conversation has attachments
	if hasAttachments {
		requests = append(requests, ConversationAttachmentsTool.ToolRequest)
	}

	// Force-include document tools when the conversation has document attachments
	if hasDocumentAttachments {
		requests = append(requests, ReadDocumentTool.ToolRequest)
		requests = append(requests, SearchDocumentTool.ToolRequest)
	}

	// Force-include retrieve_server_skill whenever the server has skills,
	// so the LLM can always reach them even if the user didn't explicitly
	// toggle the builtin in the picker.
	if skills.HasAny() {
		requests = append(requests, RetrieveServerSkillTool.ToolRequest)
	}

	// Force-include the wait tool unconditionally. The user cannot disable it
	// from the picker; the tool_loop detects calls to it and reschedules the
	// agent after the requested delay.
	requests = append(requests, WaitTool.ToolRequest)

	// Force-include send_notification iff the admin has configured an NTFY
	// server in config.json (or NTFY_* env vars). When unconfigured, the LLM
	// never sees it.
	if auth.NotificationsEnabled() {
		requests = append(requests, NotifyTool.ToolRequest)
	}

	// Force-include the device-side filesystem tools whenever the user has
	// attached a local folder to the conversation (signalled by a non-empty
	// ModelSelected.ClientFolderPath). Schema-only — the server doesn't
	// execute them, the client does.
	if hasClientFolder {
		requests = append(requests, FsClientReadToolRequest)
		requests = append(requests, FsClientWriteToolRequest)
	}

	return requests
}

// truncateWords returns the first n words of s, appending "..." if truncated.
func truncateWords(s string, n int) string {
	words := strings.Fields(s)
	if len(words) <= n {
		return s
	}
	return strings.Join(words[:n], " ") + "..."
}

// GetDisabledToolsSummary returns a compact text listing all tools that are
// available but not enabled in model.Tools. The LLM can use this to suggest
// enabling tools. Only the name and first 20 words of description are included
// to minimise token usage.
func GetDisabledToolsSummary(model utils.Model, ClientSideTools []utils.FunctionToolsRequest) string {
	selected := model.Tools
	var lines []string

	// Builtin tools
	for _, tool := range Registry {
		if tool.ToolID == ConversationAttachmentsTool.ToolID || tool.ToolID == ReadDocumentTool.ToolID || tool.ToolID == SearchDocumentTool.ToolID {
			continue
		}
		if tool.ToolID == RetrieveServerSkillTool.ToolID {
			continue
		}
		if tool.ToolID == WaitTool.ToolID {
			continue
		}
		if tool.ToolID == NotifyTool.ToolID {
			continue
		}
		selKey := tool.ToolID
		if tool.BundleName != "" {
			selKey = tool.BundleName + mcp.NamespaceSeparator + tool.ToolID
		}
		if _, ok := selected[selKey]; !ok {
			lines = append(lines, fmt.Sprintf("- %s: %s", selKey, truncateWords(tool.ToolRequest.Function.Description, 20)))
		}
	}

	// Server-side MCP tools
	for _, mcpReq := range mcp.ToolsRequests() {
		if _, ok := selected[mcpReq.Function.Name]; !ok {
			lines = append(lines, fmt.Sprintf("- %s: %s", mcpReq.Function.Name, truncateWords(mcpReq.Function.Description, 20)))
		}
	}

	// Client-side tools
	for _, tool := range ClientSideTools {
		if _, ok := selected[tool.Name]; !ok {
			lines = append(lines, fmt.Sprintf("- %s: %s", tool.Name, truncateWords(tool.Description, 20)))
		}
	}

	if len(lines) == 0 {
		return ""
	}

	return "The following tools are available but currently disabled. If you need one, ask the user to enable it:\n" + strings.Join(lines, "\n")
}
