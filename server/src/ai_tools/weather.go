package ai_tools

import (
	"github.com/azukaar/plurality/src/utils"
)

var WeatherTool = utils.AITool{
	Name:        "Weather",
	Description: "Get the weather for a location",
	ToolID:      "get_current_weather",
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "get_current_weather",
			Description: "Get the current weather in a given location",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"location": {
						Type:        "string",
						Description: "The location to get the weather for",
					},
					"unit": {
						Type:        "string",
						Description: "The location to get the weather for",
						Enum:        []string{"celsius", "fahrenheit"},
					},
				},
				Required: []string{"location", "unit"},
			},
		},
	},
	LoadingString: "Getting the weather...",
	IconURL:       "https://cdn-icons-png.flaticon.com/512/220/220221.png",
	Exec: func(args string, _ utils.Conversation) utils.MessageContent {
		argsJson := utils.ParseJson(args)

		location := argsJson["location"].(string)
		unit := argsJson["unit"].(string)

		return utils.NewTextContent("The weather in " + location + " is 25 degrees " + unit)
	},
}
