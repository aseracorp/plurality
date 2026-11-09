package ai

import (
	"strings"

	"github.com/azukaar/plurality/src/utils"
)

// bug4ToolSchemas builds a tool set of roughly `tokens` estimated tokens,
// mimicking the real MCP tool tax (~25-30k tokens of schemas per request on a
// wide toolset) that the eco trigger must account for.
func bug4ToolSchemas(tokens int) []utils.ToolsRequest {
	const n = 3
	per := tokens * 3 / n
	if per < 1 {
		per = 1
	}
	out := make([]utils.ToolsRequest, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, utils.ToolsRequest{
			Type: "function",
			Function: utils.FunctionToolsRequest{
				Name:        "tool_" + itoaB4(i),
				Description: strings.Repeat("d", per),
				Parameters: &utils.ParameterToolsRequest{
					Type: "object",
					Properties: map[string]utils.PropertyParameterToolsRequest{
						"input": {Type: "string", Description: strings.Repeat("p", 20)},
					},
				},
			},
		})
	}
	return out
}
