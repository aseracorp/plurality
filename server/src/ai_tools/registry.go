package ai_tools 

import (
	"github.com/azukaar/plurality/src/utils"

	"log"
)


var Registry = map[string]utils.AITool{
	// WeatherTool.ToolID: WeatherTool,
	WebTool.ToolID: WebTool,
	SearchTool.ToolID: SearchTool,
}

func RegisterTool(tool utils.AITool) {
	Registry[tool.ToolID] = tool
}

func GetTool(toolID string) (utils.AITool, bool) {
	tool, ok := Registry[toolID]
	return tool, ok
}

func GetRequests() []utils.ToolsRequest {
	var requests []utils.ToolsRequest 
	for _, tool := range Registry {
		requests = append(requests, tool.ToolRequest)
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
	toolRequest.InputSchema = toolRequest.Parameters[0]
	toolRequest.Parameters = nil

	return toolRequest
}

func GetClaudeRequests() []utils.FunctionToolsRequest {
	var requests []utils.FunctionToolsRequest 
	for _, tool := range Registry {
		requests = append(requests, GetClaudeSchema(tool.ToolID))
	}
	return requests
}
