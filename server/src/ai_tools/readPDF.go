package ai_tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/azukaar/plurality/src/storage"
	"github.com/azukaar/plurality/src/utils"
)

var ReadPDFTool = utils.AITool{
	Name:        "PDF Reader",
	Description: "Extract readable text from PDF attachments",
	ToolID:      "read_pdf",
	Cost:        0,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "read_pdf",
			Description: "Extract readable text content from a PDF document attached to this conversation. Use this when you see a PDF attachment placeholder. Returns the text page by page.",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"attachment_id": {
						Type:        "string",
						Description: "The attachment ID (e.g. \"att_0\") from the conversation",
					},
					"pages": {
						Type:        "string",
						Description: "Optional page selection: a single page like \"3\", a range like \"1-5\", or omit for all pages",
					},
					"max_chars": {
						Type:        "number",
						Description: "Maximum characters to return (default 10000). Use a smaller value for large documents to get an overview first.",
					},
				},
				Required: []string{"attachment_id"},
			},
		},
	},
	LoadingString: "Reading PDF...",
	IconURL:       "",
	Exec:          execReadPDF,
}

type readPDFParams struct {
	AttachmentID string  `json:"attachment_id"`
	Pages        string  `json:"pages"`
	MaxChars     float64 `json:"max_chars"`
}

func execReadPDF(_ context.Context, input string, conv utils.Conversation) utils.MessageContent {
	var params readPDFParams
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error parsing parameters: %s", err.Error()))
	}

	if params.AttachmentID == "" {
		return utils.NewTextContent("Error: attachment_id is required.")
	}

	index := utils.BuildAttachmentIndex(conv.Messages, storage.FileSizeFromURL)

	// Find the attachment
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

	// Get the content (internal URL or inline data)
	content, ok := index.ContentMap[params.AttachmentID]
	if !ok {
		return utils.NewTextContent(fmt.Sprintf("Content for attachment %s not found.", params.AttachmentID))
	}

	// Read the file from disk
	var pdfData []byte
	if storage.IsInternalURL(content) {
		data, _, err := storage.ReadBlob(content)
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error reading PDF file: %s", err.Error()))
		}
		pdfData = data
	} else {
		// Fallback: inline data URI (shouldn't happen after extraction, but handle gracefully)
		data, _, _, err := storage.ExtractBlobFromDataURI(content)
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error decoding PDF data: %s", err.Error()))
		}
		pdfData = data
	}

	maxChars := int(params.MaxChars)
	text, err := utils.ParsePDF(pdfData, params.Pages, maxChars)
	if err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error parsing PDF: %s", err.Error()))
	}

	return utils.NewTextContent(text)
}
