package ai_tools

import (
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
	return tool, ok
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
		if _, ok := selected[tool.ToolID]; ok {
			requests = append(requests, tool.ToolRequest)
		}
	}

	// Server-side MCP tools (from data/mcp.json). Name collisions with
	// ClientSideTools are resolved in the tool loop's categorizer — here
	// we just advertise whatever is selected in model.Tools.
	for _, mcpReq := range mcp.ToolsRequests() {
		if _, ok := selected[mcpReq.Function.Name]; ok {
			requests = append(requests, mcpReq)
		}
	}

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
