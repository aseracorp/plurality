package ai_tools

import (
	"strings"

	"github.com/azukaar/plurality/src/utils"

	"log"
)

var Registry = map[string]utils.AITool{
	// WeatherTool.ToolID: WeatherTool,
	// NewsSearchTool.ToolID: NewsSearchTool,
	DiceRollTool.ToolID:    DiceRollTool,
	WebTool.ToolID:         WebTool,
	SearchTool.ToolID:      SearchTool,
	PlaceSearchTool.ToolID: PlaceSearchTool,
	ImageGenTool.ToolID:    ImageGenTool,
}

func RegisterTool(tool utils.AITool) {
	Registry[tool.ToolID] = tool
}

func GetTool(toolID string) (utils.AITool, bool) {
	tool, ok := Registry[toolID]
	return tool, ok
}

func ShouldStripResponse(content string) bool {
	return strings.Contains(content, "base64,")
}

func GetRequests(model utils.Model, ClientSideTools []utils.FunctionToolsRequest) []utils.ToolsRequest {
	var requests []utils.ToolsRequest
	var selected = model.Tools

	for _, tool := range Registry {
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

	return requests
}

func GetClaudeSchema(toolID string) utils.FunctionToolsRequest {
	tool, ok := GetTool(toolID)
	if !ok {
		log.Printf("Tool with ID %s not found", toolID)
		return utils.FunctionToolsRequest{}
	}

	toolRequest := tool.ToolRequest.Function
	toolRequest.InputSchema = toolRequest.Parameters
	toolRequest.Parameters = nil

	return toolRequest
}

func ConvertToClaudeSchema(requests utils.FunctionToolsRequest) utils.FunctionToolsRequest {
	toolRequest := requests
	toolRequest.InputSchema = toolRequest.Parameters
	toolRequest.Parameters = nil

	return toolRequest
}

func GetClaudeRequests(model utils.Model, ClientSideTools []utils.FunctionToolsRequest) []utils.FunctionToolsRequest {
	var requests = []utils.FunctionToolsRequest{}
	var selected = model.Tools

	for _, tool := range Registry {
		if utils.ContainsString(selected, tool.ToolID) {
			requests = append(requests, GetClaudeSchema(tool.ToolID))
		}
	}

	for _, tool := range ClientSideTools {
		if utils.ContainsString(selected, tool.Name) {
			requests = append(requests, ConvertToClaudeSchema(tool))
		}
	}

	return requests
}
