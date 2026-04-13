package ai_tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/azukaar/plurality/src/storage"
	"github.com/azukaar/plurality/src/utils"
)

var SearchPDFTool = utils.AITool{
	Name:        "PDF Search",
	Description: "Search PDF attachments with regex",
	ToolID:      "search_pdf",
	Cost:        0,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "search_pdf",
			Description: "Search a PDF document attached to this conversation using a regex pattern. Returns matching text with page numbers and surrounding context. Useful for finding specific information in large documents without reading them entirely.",
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
	LoadingString: "Searching PDF...",
	IconURL:       "",
	Exec:          execSearchPDF,
}

type searchPDFParams struct {
	AttachmentID string  `json:"attachment_id"`
	Pattern      string  `json:"pattern"`
	MaxResults   float64 `json:"max_results"`
}

func execSearchPDF(_ context.Context, input string, conv utils.Conversation) utils.MessageContent {
	var params searchPDFParams
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error parsing parameters: %s", err.Error()))
	}

	if params.AttachmentID == "" {
		return utils.NewTextContent("Error: attachment_id is required.")
	}
	if params.Pattern == "" {
		return utils.NewTextContent("Error: pattern is required.")
	}

	index := utils.BuildAttachmentIndex(conv.Messages, storage.FileSizeFromURL)

	var att *utils.AttachmentMeta
	for _, item := range index.Items {
		if item.ID == params.AttachmentID {
			att = &item
			break
		}
	}
	if att == nil {
		return utils.NewTextContent(fmt.Sprintf("Attachment %s not found.", params.AttachmentID))
	}

	if att.Type != "pdf" {
		return utils.NewTextContent(fmt.Sprintf("Attachment %s is not a PDF (type: %s). This tool only supports PDF documents.", params.AttachmentID, att.Type))
	}

	content, ok := index.ContentMap[params.AttachmentID]
	if !ok {
		return utils.NewTextContent(fmt.Sprintf("Content for attachment %s not found.", params.AttachmentID))
	}

	var pdfData []byte
	if storage.IsInternalURL(content) {
		data, _, err := storage.ReadBlob(content)
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error reading PDF file: %s", err.Error()))
		}
		pdfData = data
	} else {
		data, _, _, err := storage.ExtractBlobFromDataURI(content)
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error decoding PDF data: %s", err.Error()))
		}
		pdfData = data
	}

	maxResults := int(params.MaxResults)
	matches, err := utils.SearchPDF(pdfData, params.Pattern, maxResults)
	if err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error searching PDF: %s", err.Error()))
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
