package ai_tools

import (
	"context"

	"github.com/azukaar/plurality/src/skills"
	"github.com/azukaar/plurality/src/utils"
)

// RetrieveServerSkillTool reads a file from the server's shared skills library.
// Registered in the global Registry only when skills.HasAny().
var RetrieveServerSkillTool = utils.AITool{
	Name:        "Retrieve Server Skill",
	Description: "Retrieve instructions from the server's shared skills library.",
	ToolID:      "retrieve_server_skill",
	Cost:        0,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "retrieve_server_skill",
			Description: "Retrieve a skill's instructions from the server's shared skills library.",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"skill_name": {
						Type:        "string",
						Description: "Name of the skill to retrieve",
					},
					"file_name": {
						Type:        "string",
						Description: "Optional file to read from the skill folder (defaults to SKILL.md)",
					},
				},
				Required: []string{"skill_name"},
			},
		},
	},
	LoadingString: "Loading skill {{skill_name}}",
	Exec: func(_ context.Context, argsJSON string, _ utils.Conversation) utils.MessageContent {
		args := utils.ParseJsonString(argsJSON)
		skillName := args["skill_name"]
		fileName := args["file_name"]
		if skillName == "" {
			return utils.NewTextContent("Error: skill_name is required")
		}
		content, err := skills.ReadFile(skillName, fileName)
		if err != nil {
			return utils.NewTextContent("Error: " + err.Error())
		}
		return utils.NewTextContent(content)
	},
}
