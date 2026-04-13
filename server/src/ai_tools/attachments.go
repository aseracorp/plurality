package ai_tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/azukaar/plurality/src/utils"
)

// ConversationAttachmentsTool allows the AI to retrieve attachments from earlier
// in the conversation that have been replaced with placeholders to save context.
var ConversationAttachmentsTool = utils.AITool{
	Name:        "Conversation Attachments",
	Description: "Retrieve attachments from the conversation",
	ToolID:      "conversation_attachments",
	Cost:        0,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name: "conversation_attachments",
			Description: "Retrieve attachments (images, documents, code snippets) from earlier in this conversation that were omitted to save context. WARNING: Use sparingly and avoid calling multiple times in a row. Two modes: 'list' returns metadata for all attachments, 'get' retrieves content for up to 5 attachments with optional byte-range slicing.",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"mode": {
						Type:        "string",
						Description: "The operation mode: 'list' to list all attachments with metadata (id, type, extension, size), 'get' to retrieve specific attachment content",
						Enum:        []string{"list", "get"},
					},
					"attachments": {
						Type:        "string",
						Description: "JSON array for 'get' mode only. Each element: {\"id\": \"att_0\", \"from\": 0, \"to\": 1000}. 'from' and 'to' are optional byte offsets for partial retrieval. Maximum 5 attachments per call.",
					},
				},
				Required: []string{"mode"},
			},
		},
	},
	LoadingString: "Retrieving attachments...",
	IconURL:       "",
	Exec:          execConversationAttachments,
}

type attachmentGetRequest struct {
	ID   string `json:"id"`
	From *int   `json:"from,omitempty"`
	To   *int   `json:"to,omitempty"`
}

func execConversationAttachments(input string, conv utils.Conversation) utils.MessageContent {
	var params map[string]string
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error parsing parameters: %s", err.Error()))
	}

	mode := params["mode"]
	utils.Log("[Attachments] conversation_attachments called with mode=%s", mode)

	index := utils.BuildAttachmentIndex(conv.Messages)

	if len(index.Items) == 0 {
		utils.Log("[Attachments] No attachments found")
		return utils.NewTextContent("No attachments found in this conversation.")
	}

	switch mode {
	case "list":
		utils.Log("[Attachments] Listing %d attachment(s)", len(index.Items))
		return handleList(index)
	case "get":
		utils.Log("[Attachments] Get request: %s", params["attachments"])
		return handleGet(index, params["attachments"])
	default:
		return utils.NewTextContent("Invalid mode. Use 'list' or 'get'.")
	}
}

func handleList(index utils.AttachmentIndex) utils.MessageContent {
	result, err := json.Marshal(index.Items)
	if err != nil {
		return utils.NewTextContent("Error serializing attachment list.")
	}
	return utils.NewTextContent(string(result))
}

func handleGet(index utils.AttachmentIndex, attachmentsJSON string) utils.MessageContent {
	if attachmentsJSON == "" {
		return utils.NewTextContent("Error: 'attachments' parameter is required for 'get' mode. Provide a JSON array like [{\"id\": \"att_0\"}].")
	}

	var requests []attachmentGetRequest
	if err := json.Unmarshal([]byte(attachmentsJSON), &requests); err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error parsing 'attachments' parameter: %s. Expected JSON array like [{\"id\": \"att_0\"}].", err.Error()))
	}

	if len(requests) == 0 {
		return utils.NewTextContent("Error: empty attachments array.")
	}

	if len(requests) > 5 {
		return utils.NewTextContent("Error: maximum 5 attachments per request. Please reduce the number of attachments.")
	}

	// Build multi-part content: images become image_url parts, text stays as text
	var parts []utils.ContentPart
	var notFound []string

	for _, req := range requests {
		content, ok := index.ContentMap[req.ID]
		if !ok {
			notFound = append(notFound, req.ID)
			continue
		}

		// Find the attachment metadata to determine type
		var attMeta *utils.AttachmentMeta
		for _, item := range index.Items {
			if item.ID == req.ID {
				attMeta = &item
				break
			}
		}

		// Apply partial retrieval bounds
		total := len(content)
		from := 0
		to := total
		if req.From != nil && *req.From >= 0 && *req.From < total {
			from = *req.From
		}
		if req.To != nil && *req.To > from && *req.To <= total {
			to = *req.To
		}
		sliced := content[from:to]

		if attMeta != nil && attMeta.Type == "image_url" {
			// Return as a proper image content part so the AI can see it
			parts = append(parts, utils.ContentPart{
				Type:     "image_url",
				ImageURL: &utils.ContentImageURL{URL: sliced},
			})
			// Add a text label so the AI knows which attachment this is
			parts = append(parts, utils.ContentPart{
				Type: "text",
				Text: fmt.Sprintf("[Retrieved image: %s (%d bytes)]", req.ID, total),
			})
		} else {
			// Text-based attachment (snippet, file, etc.)
			label := req.ID
			if attMeta != nil && attMeta.Ext != "" {
				label = fmt.Sprintf("%s (%s)", req.ID, attMeta.Ext)
			}
			if from != 0 || to != total {
				parts = append(parts, utils.ContentPart{
					Type: "text",
					Text: fmt.Sprintf("[Attachment %s, bytes %d-%d of %d]:\n%s", label, from, to, total, sliced),
				})
			} else {
				parts = append(parts, utils.ContentPart{
					Type: "text",
					Text: fmt.Sprintf("[Attachment %s]:\n%s", label, sliced),
				})
			}
		}
	}

	if len(notFound) > 0 {
		parts = append([]utils.ContentPart{{
			Type: "text",
			Text: fmt.Sprintf("Attachments not found: %s", strings.Join(notFound, ", ")),
		}}, parts...)
	}

	if len(parts) == 0 {
		return utils.NewTextContent("No matching attachments found.")
	}

	return utils.NewPartsContent(parts)
}
