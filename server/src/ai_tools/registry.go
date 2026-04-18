package ai_tools

import (
	"fmt"
	"strings"

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
	// Strip namespace prefix for bundled builtins (e.g. "conversations__search_conversations" → "search_conversations").
	if idx := strings.Index(toolID, mcp.NamespaceSeparator); idx >= 0 {
		tool, ok = Registry[toolID[idx+len(mcp.NamespaceSeparator):]]
		return tool, ok
	}
	return utils.AITool{}, false
}

func ShouldStripResponse(content string) bool {
	return strings.HasPrefix(content, "base64,")
}

func GetRequests(model utils.Model, ClientSideTools []utils.FunctionToolsRequest, hasAttachments bool, hasDocumentAttachments bool) []utils.ToolsRequest {
	var requests []utils.ToolsRequest
	var selected = model.Tools

	for _, tool := range Registry {
		if tool.ToolID == ConversationAttachmentsTool.ToolID || tool.ToolID == ReadDocumentTool.ToolID || tool.ToolID == SearchDocumentTool.ToolID {
			continue // handled separately below
		}
		if tool.ToolID == RetrieveServerSkillTool.ToolID {
			continue // force-included below when skills exist
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

	return requests
}
