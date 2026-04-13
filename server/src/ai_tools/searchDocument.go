package ai_tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/azukaar/plurality/src/docsupport"
	"github.com/azukaar/plurality/src/utils"
)

var SearchDocumentTool = utils.AITool{
	Name:        "Document Search",
	Description: "Search document attachments (PDF, DOCX, XLSX, PPTX) with regex",
	ToolID:      "search_document",
	Cost:        0,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "search_document",
			Description: "Search a document (PDF, DOCX, XLSX, PPTX) attached to this conversation using a regex pattern. Returns matching text with surrounding context. Useful for finding specific information in large documents without reading them entirely.",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"attachment_id": {
						Type:        "string",
						Description: "The attachment ID (e.g. \"att_0\") from the conversation",
					},
					"pattern": {
						Type:        "string",
						Description: "Regex pattern to search for (e.g. \"total.*revenue\", \"\\d{4}-\\d{2}-\\d{2}\" for dates, or a simple string like \"conclusion\")",
					},
					"max_results": {
						Type:        "number",
						Description: "Maximum number of matches to return (default 50)",
					},
				},
				Required: []string{"attachment_id", "pattern"},
			},
		},
	},
	LoadingString: "Searching document...",
	IconURL:       "",
	Exec:          execSearchDocument,
}

type searchDocumentParams struct {
	AttachmentID string  `json:"attachment_id"`
	Pattern      string  `json:"pattern"`
	MaxResults   float64 `json:"max_results"`
}

func execSearchDocument(_ context.Context, input string, conv utils.Conversation) utils.MessageContent {
	var params searchDocumentParams
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error parsing parameters: %s", err.Error()))
	}

	if params.AttachmentID == "" {
		return utils.NewTextContent("Error: attachment_id is required.")
	}
	if params.Pattern == "" {
		return utils.NewTextContent("Error: pattern is required.")
	}

	att, data, errContent := resolveDocumentAttachment(conv, params.AttachmentID)
	if errContent != nil {
		return *errContent
	}

	maxResults := int(params.MaxResults)
	matches, err := docsupport.SearchDocument(data, att.Ext, params.Pattern, maxResults)
	if err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error searching document: %s", err.Error()))
	}

	if len(matches) == 0 {
		return utils.NewTextContent(fmt.Sprintf("No matches found for pattern \"%s\".", params.Pattern))
	}

	result, err := json.Marshal(matches)
	if err != nil {
		return utils.NewTextContent("Error serializing search results.")
	}

	return utils.NewTextContent(fmt.Sprintf("Found %d match(es):\n%s", len(matches), string(result)))
}
