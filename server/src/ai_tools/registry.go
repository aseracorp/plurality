package ai_tools

import (
	"strings"

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
		if utils.ContainsString(selected, tool.ToolID) {
			requests = append(requests, tool.ToolRequest)
		}
	}

	for _, tool := range ClientSideTools {
		if utils.ContainsString(selected, tool.Name) {
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

	return requests
}
