package ai_tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/azukaar/plurality/src/docsupport"
	"github.com/azukaar/plurality/src/utils"
)

var ReadDocumentTool = utils.AITool{
	Name:        "Document Reader",
	Description: "Extract readable text from document attachments (PDF, DOCX, XLSX, PPTX)",
	ToolID:      "read_document",
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "read_document",
			Description: "Extract text from a document attachment (PDF, DOCX, XLSX, PPTX).",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"attachment_id": {
						Type:        "string",
						Description: "Attachment ID (e.g. 'att_0')",
					},
					"pages": {
						Type:        "string",
						Description: "Page selection for PDFs: '3', '1-5', or omit for all",
					},
					"max_chars": {
						Type:        "number",
						Description: "Max characters to return (default 10000)",
					},
				},
				Required: []string{"attachment_id"},
			},
		},
	},
	LoadingString: "Reading document...",
	IconURL:       "",
	Exec:          execReadDocument,
}

type readDocumentParams struct {
	AttachmentID string  `json:"attachment_id"`
	Pages        string  `json:"pages"`
	MaxChars     float64 `json:"max_chars"`
}

func execReadDocument(_ context.Context, input string, conv utils.Conversation) utils.MessageContent {
	var params readDocumentParams
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error parsing parameters: %s", err.Error()))
	}

	if params.AttachmentID == "" {
		return utils.NewTextContent("Error: attachment_id is required.")
	}

	att, data, errContent := resolveDocumentAttachment(conv, params.AttachmentID)
	if errContent != nil {
		return *errContent
	}

	maxChars := int(params.MaxChars)
	text, err := docsupport.ParseDocument(data, att.Ext, params.Pages, maxChars)
	if err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error parsing document: %s", err.Error()))
	}

	return utils.NewTextContent(text)
}
